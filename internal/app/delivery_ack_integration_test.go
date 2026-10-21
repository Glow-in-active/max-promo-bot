//go:build integration

package app_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"promo/internal/app"
	"promo/internal/platform/maxapi"
)

func TestMalformedDeliveryAcknowledgementDoesNotStartClaimDeadline(t *testing.T) {
	h := setup(t, "drawable")
	ctx := context.Background()
	var validAcknowledgement atomic.Bool
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/messages" {
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if validAcknowledgement.Load() {
			_, _ = io.WriteString(w, `{"message":{"body":{"mid":"confirmed-message"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"message":{"body":{"mid":" "}}}`)
	}))
	defer provider.Close()
	sender, err := maxapi.New(maxapi.Config{Token: "local-test-token", BaseURL: provider.URL})
	must(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.service = app.New(h.store, sender, h.clock, firstCandidate{}, app.WithLogger(logger))
	_, err = h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "test-admin")
	must(t, err)
	draw, err := h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "test-admin")
	must(t, err)
	offerID := h.offers(t, h.run.DrawID)[0].ID

	processUntil := func(want string) {
		t.Helper()
		workerCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			h.service.Work(workerCtx, logger)
			close(done)
		}()
		defer func() { cancel(); <-done }()
		timeout := time.NewTimer(4 * time.Second)
		defer timeout.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			offer, err := h.store.Offer(ctx, offerID)
			must(t, err)
			if offer.DeliveryState == want {
				return
			}
			select {
			case <-timeout.C:
				t.Fatalf("notification did not reach %q: %+v", want, offer)
			case <-ticker.C:
			}
		}
	}
	processUntil("failed")
	failed, err := h.store.Offer(ctx, offerID)
	must(t, err)
	if failed.Status != "pending" || failed.SentAt != nil || failed.Deadline != nil || failed.DeliveryError != "delivery uncertain" {
		t.Fatalf("malformed acknowledgement started the claim period: %+v", failed)
	}
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if len(messages) != 1 || messages[0].Attempts != 1 || calls.Load() != 1 {
		t.Fatalf("uncertain delivery was retried automatically: messages=%+v calls=%d", messages, calls.Load())
	}
	h.clock.Set(h.clock.Now().Add(draw.ClaimTTL + time.Hour))
	must(t, h.service.ExpireOffer(ctx, offerID))
	if len(h.offers(t, h.run.DrawID)) != 1 {
		t.Fatal("an unconfirmed notification passed the prize to reserve")
	}

	validAcknowledgement.Store(true)
	must(t, h.service.RetryDelivery(ctx, offerID, "test-admin"))
	processUntil("delivered")
	delivered, err := h.store.Offer(ctx, offerID)
	must(t, err)
	if delivered.Status != "notified" || delivered.SentAt == nil || !delivered.SentAt.Equal(h.clock.Now()) || delivered.Deadline == nil || !delivered.Deadline.Equal(h.clock.Now().Add(draw.ClaimTTL)) || delivered.DeliveryError != "" || calls.Load() != 2 {
		t.Fatalf("confirmed retry did not start a complete claim period: %+v calls=%d", delivered, calls.Load())
	}
}
