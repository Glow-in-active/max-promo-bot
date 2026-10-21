//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/app"
	"promo/internal/domain"
	"promo/internal/platform/maxapi"
)

func TestOutboxDeliversThousandIndependentRecipients(t *testing.T) {
	const total = 1000
	h := setupAt(t, "standard", time.Now().UTC())
	ctx := context.Background()
	must(t, h.store.Within(ctx, func(tx app.Tx) error {
		for i := range total {
			user := int64(8_000_000_000 + i)
			if _, err := tx.UserForUpdate(ctx, user, h.clock.Now()); err != nil {
				return err
			}
			id := fmt.Sprintf("load-message-%04d", i)
			message := domain.Message{ID: id, CampaignID: h.run.CampaignID, UserID: user, Text: id}
			if err := tx.Enqueue(ctx, message, id, h.clock.Now()); err != nil {
				return err
			}
		}
		return nil
	}))
	var mu sync.Mutex
	received := map[int64]string{}
	var requestTimes []time.Time
	var duplicates int
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/messages" || r.Header.Get("Authorization") != "local-load-token" {
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		user, err := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
		var message struct {
			Text string `json:"text"`
		}
		if err != nil || json.NewDecoder(r.Body).Decode(&message) != nil || message.Text != fmt.Sprintf("load-message-%04d", user-8_000_000_000) {
			t.Error("provider received a mismatched recipient or message")
			http.Error(w, "invalid message", http.StatusBadRequest)
			return
		}
		mu.Lock()
		if _, exists := received[user]; exists {
			duplicates++
		}
		received[user] = message.Text
		requestTimes = append(requestTimes, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"message":{"body":{"mid":"local-load-delivery"}}}`)
	}))
	t.Cleanup(provider.Close)
	sender, err := maxapi.New(maxapi.Config{Token: "local-load-token", BaseURL: provider.URL})
	must(t, err)
	h.service = app.New(h.store, sender, h.clock, firstCandidate{})
	started := time.Now()
	stop := startLoadWorkers(t, h)
	outbox := pgx.Identifier{h.name, "outbox"}.Sanitize()
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var delivered, attempts int
		must(t, h.observer.QueryRow(ctx, "SELECT count(*) FILTER (WHERE state='delivered'), count(*) FILTER (WHERE attempts<>1) FROM "+outbox).Scan(&delivered, &attempts))
		if delivered == total {
			if attempts != 0 {
				t.Fatalf("completed load required unexpected retry attempts: %d", attempts)
			}
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("independent recipient backlog stalled: delivered=%d/%d", delivered, total)
		case <-ticker.C:
		}
	}
	elapsed := time.Since(started)
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(received) != total || duplicates != 0 {
		t.Fatalf("load lost or duplicated delivery: recipients=%d duplicates=%d", len(received), duplicates)
	}
	sort.Slice(requestTimes, func(i, j int) bool { return requestTimes[i].Before(requestTimes[j]) })
	for i := 30; i < len(requestTimes); i++ {
		if gap := requestTimes[i].Sub(requestTimes[i-30]); gap < time.Second-5*time.Millisecond {
			t.Fatalf("outbox exceeded 30 MAX requests per second: 31 requests in %v", gap)
		}
	}
	t.Logf("isolated PostgreSQL + HTTP provider: messages=%d recipients=%d elapsed=%s rate=%.1f messages/s", total, len(received), elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds())
}

func TestOutboxBusyRecipientDoesNotOccupyOtherWorkers(t *testing.T) {
	const backlog = 100
	h := setupAt(t, "standard", time.Now().UTC())
	ctx := context.Background()
	busy, independent := h.run.UserIDs[0], h.run.UserIDs[1]
	must(t, h.store.Within(ctx, func(tx app.Tx) error {
		for i := range backlog + 1 {
			user := busy
			if i == backlog {
				user = independent
			}
			id := fmt.Sprintf("busy-message-%03d", i)
			if err := tx.Enqueue(ctx, domain.Message{ID: id, CampaignID: h.run.CampaignID, UserID: user, Text: id}, id, h.clock.Now()); err != nil {
				return err
			}
		}
		return nil
	}))
	busyStarted := make(chan struct{})
	independentSent := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseBusy := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseBusy)
	var mu sync.Mutex
	busyCalls := 0
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
		switch user {
		case busy:
			mu.Lock()
			busyCalls++
			first := busyCalls == 1
			mu.Unlock()
			if first {
				close(busyStarted)
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		case independent:
			close(independentSent)
		default:
			t.Errorf("unexpected recipient: %d", user)
			http.Error(w, "invalid recipient", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"message":{"body":{"mid":"local-mixed-delivery"}}}`)
	}))
	t.Cleanup(provider.Close)
	sender, err := maxapi.New(maxapi.Config{Token: "local-load-token", BaseURL: provider.URL})
	must(t, err)
	h.service = app.New(h.store, sender, h.clock, firstCandidate{}, app.WithOutboxWorkers(2))
	stop := startLoadWorkers(t, h)
	for _, event := range []<-chan struct{}{busyStarted, independentSent} {
		select {
		case <-event:
		case <-time.After(3 * time.Second):
			t.Fatal("busy recipient blocked delivery to an independent recipient")
		}
	}
	outbox := pgx.Identifier{h.name, "outbox"}.Sanitize()
	awaitWorkerProgress(t, "the independent recipient delivery", func() bool {
		var delivered bool
		must(t, h.observer.QueryRow(ctx, "SELECT state='delivered' FROM "+outbox+" WHERE user_id=$1", independent).Scan(&delivered))
		return delivered
	})
	var pending, processing int
	must(t, h.observer.QueryRow(ctx, "SELECT count(*) FILTER (WHERE state='pending'), count(*) FILTER (WHERE state='processing') FROM "+outbox+" WHERE user_id=$1", busy).Scan(&pending, &processing))
	mu.Lock()
	calls := busyCalls
	mu.Unlock()
	if pending != backlog-1 || processing != 1 || calls != 1 {
		t.Fatalf("one recipient consumed parallel workers: pending=%d processing=%d provider calls=%d", pending, processing, calls)
	}
	stop()
	releaseBusy()
}

func startLoadWorkers(t *testing.T, h *harness) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.service.Work(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); close(done) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("outbox workers did not stop after cancellation")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}
