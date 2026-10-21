//go:build integration

package app_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/app"
	"promo/internal/platform/maxapi"
)

func TestApplicationProviderRetryAfterControlsDurableOutbox(t *testing.T) {
	h := setup(t, "drawable")
	ctx := context.Background()
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/messages" {
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "300")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"code":"rate.limit"}`)
			return
		}
		_, _ = io.WriteString(w, `{"message":{"body":{"mid":"sent"}}}`)
	}))
	defer provider.Close()
	sender, err := maxapi.New(maxapi.Config{Token: "local-test-token", BaseURL: provider.URL})
	must(t, err)
	h.service = app.New(h.store, sender, h.clock, firstCandidate{})
	_, err = h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	draw, err := h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	offer := h.offers(t, h.run.DrawID)[0]
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if len(messages) != 1 || messages[0].OfferID != offer.ID {
		t.Fatalf("expected one durable prize notification: %+v", messages)
	}
	messageID := messages[0].ID
	start := h.clock.Now()
	table := pgx.Identifier{h.name, "outbox"}.Sanitize()
	state := func(id string) (string, int, time.Time) {
		var status string
		var attempts int
		var next time.Time
		must(t, h.observer.QueryRow(ctx, "SELECT state,attempts,next_attempt_at FROM "+table+" WHERE id=$1", id).Scan(&status, &attempts, &next))
		return status, attempts, next
	}
	processUntil := func(id, want string) {
		workerCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { h.service.Work(workerCtx, slog.New(slog.NewTextHandler(io.Discard, nil))); close(done) }()
		defer func() { cancel(); <-done }()
		timeout := time.NewTimer(4 * time.Second)
		defer timeout.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			status, _, _ := state(id)
			if status == want {
				return
			}
			select {
			case <-timeout.C:
				t.Fatalf("outbox %s did not reach %q", id, want)
			case <-ticker.C:
			}
		}
	}
	processUntil(messageID, "retry")
	status, attempts, next := state(messageID)
	if status != "retry" || attempts != 1 || !next.Equal(start.Add(300*time.Second)) {
		t.Fatalf("rate limit not persisted: state=%s attempts=%d next=%s", status, attempts, next)
	}
	offer, err = h.store.Offer(ctx, offer.ID)
	must(t, err)
	if offer.Status != "pending" || offer.SentAt != nil || offer.Deadline != nil {
		t.Fatalf("rate-limited notification started the claim deadline: %+v", offer)
	}

	h.clock.Set(start.Add(300*time.Second - time.Nanosecond))
	message, err := h.store.TakeMessage(ctx, h.clock.Now())
	must(t, err)
	if message != nil || calls.Load() != 1 {
		t.Fatal("outbox retried before Retry-After")
	}
	h.clock.Set(start.Add(300 * time.Second))
	processUntil(messageID, "delivered")
	_, attempts, _ = state(messageID)
	if attempts != 2 || calls.Load() != 2 {
		t.Fatalf("unexpected retry count after cooldown: attempts=%d calls=%d", attempts, calls.Load())
	}
	offer, err = h.store.Offer(ctx, offer.ID)
	must(t, err)
	if offer.Status != "notified" || offer.SentAt == nil || !offer.SentAt.Equal(h.clock.Now()) || offer.Deadline == nil || !offer.Deadline.Equal(h.clock.Now().Add(draw.ClaimTTL)) {
		t.Fatalf("successful provider acknowledgement did not start the complete claim period: %+v", offer)
	}
	sent, deadline := *offer.SentAt, *offer.Deadline

	h.clock.Set(h.clock.Now().Add(time.Minute))
	_, err = h.observer.Exec(ctx, "UPDATE "+table+" SET state='processing',lease_until=$2 WHERE id=$1", messageID, h.clock.Now().Add(-time.Second))
	must(t, err)
	processUntil(messageID, "delivered")
	offer, err = h.store.Offer(ctx, offer.ID)
	must(t, err)
	_, attempts, _ = state(messageID)
	if calls.Load() != 3 || attempts != 3 || !offer.SentAt.Equal(sent) || !offer.Deadline.Equal(deadline) {
		t.Fatalf("recovered delivery extended the deadline: offer=%+v attempts=%d calls=%d", offer, attempts, calls.Load())
	}
	fresh := h.execute(t, h.command("start"))
	processUntil(fresh.ID, "delivered")
	if calls.Load() != 4 {
		t.Fatal("a completed retry prevented a fresh notification from being delivered: " + strconv.Itoa(int(calls.Load())))
	}
}
