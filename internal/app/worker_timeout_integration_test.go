//go:build integration

package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/app"
	"promo/internal/domain"
)

func timeoutHarness(t *testing.T, scenario string) *harness {
	t.Helper()
	now := time.Now().UTC().Truncate(24 * time.Hour).Add(36 * time.Hour)
	h := setupAt(t, scenario, now)
	h.service = app.New(h.store, h.messenger, h.clock, firstCandidate{}, app.WithWorkTimeout(300*time.Millisecond))
	return h
}

func startTimeoutWorkers(t *testing.T, h *harness) func() {
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
			case <-time.After(2 * time.Second):
				t.Error("workers did not stop after cancellation")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func awaitWorkerProgress(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !ready() {
		select {
		case <-deadline.C:
			t.Fatal("worker did not complete " + description)
		case <-ticker.C:
		}
	}
}

func inboxState(t *testing.T, h *harness, id string) (string, int, *time.Time) {
	t.Helper()
	var state string
	var attempts int
	var lease *time.Time
	query := "SELECT state,attempts,lease_until FROM " + pgx.Identifier{h.name, "inbox"}.Sanitize() + " WHERE id=$1"
	must(t, h.observer.QueryRow(context.Background(), query, id).Scan(&state, &attempts, &lease))
	return state, attempts, lease
}

func TestInboxTimeoutAllowsAnotherUserAndRecoversTheLease(t *testing.T) {
	h := timeoutHarness(t, "standard")
	ctx := context.Background()
	blocked := h.command("promo")
	independent := h.command("promo")
	independent.UserID = h.run.UserIDs[1]
	for _, command := range []domain.Command{blocked, independent} {
		inserted, err := h.store.PutInbox(ctx, command, command.ID)
		must(t, err)
		if !inserted {
			t.Fatal("fixture event was not queued")
		}
	}
	release := h.holdLock(t, func(tx app.Tx) error { _, err := tx.UserForUpdate(ctx, blocked.UserID, h.clock.Now()); return err })
	startTimeoutWorkers(t, h)
	h.waitForLock(t, "FROM users")
	awaitWorkerProgress(t, "the independent inbox event while another user is locked", func() bool {
		state, _, _ := inboxState(t, h, independent.ID)
		return state == "done"
	})
	state, attempts, lease := inboxState(t, h, blocked.ID)
	if state != "processing" || attempts != 1 || lease == nil {
		t.Fatalf("timed-out event lost its recoverable lease: state=%s attempts=%d lease=%v", state, attempts, lease)
	}
	active, err := h.store.Activities(ctx, h.run.CampaignID, blocked.UserID)
	must(t, err)
	stats, err := h.store.Stats(ctx, h.run.CampaignID)
	must(t, err)
	if len(active) != 0 || stats.PromoIssued != 1 || stats.PromoRequests != 1 {
		t.Fatalf("timed-out event committed partial activity: active=%+v stats=%+v", active, stats)
	}
	release()
	h.clock.Set(lease.Add(time.Millisecond))
	awaitWorkerProgress(t, "the recovered inbox event and both deliveries", func() bool {
		state, _, _ := inboxState(t, h, blocked.ID)
		messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
		must(t, err)
		return state == "done" && len(messages) == 2 && messages[0].State == "delivered" && messages[1].State == "delivered"
	})
	state, attempts, lease = inboxState(t, h, blocked.ID)
	stats, err = h.store.Stats(ctx, h.run.CampaignID)
	must(t, err)
	active, err = h.store.Activities(ctx, h.run.CampaignID, blocked.UserID)
	must(t, err)
	if state != "done" || attempts != 2 || lease != nil || len(active) != 1 || stats.PromoIssued != 2 || stats.PromoRequests != 2 {
		t.Fatalf("lease recovery duplicated or lost work: state=%s attempts=%d lease=%v active=%+v stats=%+v", state, attempts, lease, active, stats)
	}
}

func TestOutboxTimeoutAllowsAnotherMessageAndRecoversTheLease(t *testing.T) {
	h := timeoutHarness(t, "drawable")
	ctx := context.Background()
	_, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	_, err = h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	offer := h.offers(t, h.run.DrawID)[0]
	independent := h.command("start")
	independent.UserID = h.run.UserIDs[1]
	reply := h.execute(t, independent)
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if len(messages) != 2 || messages[0].OfferID != offer.ID || messages[1].ID != reply.ID {
		t.Fatalf("fixture must queue the prize before the independent reply: %+v", messages)
	}
	blockedID := messages[0].ID
	release := h.holdLock(t, func(tx app.Tx) error { return tx.LockSlot(ctx, offer.SlotID) })
	startTimeoutWorkers(t, h)
	h.waitForLock(t, "FROM slots")
	awaitWorkerProgress(t, "the independent reply while the prize slot is locked", func() bool {
		messages, err = h.store.Messages(ctx, h.run.CampaignID, "")
		must(t, err)
		return messages[1].State == "delivered"
	})
	awaitWorkerProgress(t, "the cancelled prize confirmation", func() bool {
		var waiting bool
		must(t, h.observer.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE $2
		)`, h.name, "%FROM slots%").Scan(&waiting))
		return !waiting
	})
	if messages[0].State != "processing" || messages[0].Attempts != 1 {
		t.Fatalf("timed-out delivery lost its recoverable lease: %+v", messages[0])
	}
	before, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if before.Status != "pending" || before.SentAt != nil || before.Deadline != nil {
		t.Fatalf("canceled delivery confirmation committed success: %+v", before)
	}
	var lease time.Time
	must(t, h.observer.QueryRow(ctx, "SELECT lease_until FROM "+pgx.Identifier{h.name, "outbox"}.Sanitize()+" WHERE id=$1", blockedID).Scan(&lease))
	release()
	h.clock.Set(lease.Add(time.Millisecond))
	awaitWorkerProgress(t, "the recovered prize delivery", func() bool {
		messages, err = h.store.Messages(ctx, h.run.CampaignID, "")
		must(t, err)
		return messages[0].State == "delivered"
	})
	after, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if messages[0].Attempts != 2 || messages[1].Attempts != 1 || after.Status != "notified" || after.SentAt == nil || !after.SentAt.Equal(h.clock.Now()) || after.Deadline == nil || !after.Deadline.Equal(h.clock.Now().Add(24*time.Hour)) {
		t.Fatalf("delivery recovery lost attempts or the full claim period: messages=%+v offer=%+v", messages, after)
	}
}

func startTimeoutTick(t *testing.T, h *harness) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result error
	go func() { result = h.service.Tick(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("scheduler did not stop after cancellation")
		}
	})
	return func() error {
		t.Helper()
		select {
		case <-done:
			return result
		case <-time.After(2 * time.Second):
			t.Fatal("one blocked scheduler item prevented independent work")
			return nil
		}
	}
}

func TestSchedulerDrawTimeoutStillRunsOtherDrawsAndExpiresOffers(t *testing.T) {
	h := timeoutHarness(t, "drawable")
	ctx := context.Background()
	independent, err := h.store.CurrentDay(ctx, h.run.CampaignID, h.clock.Now())
	must(t, err)
	h.clock.Set(independent.EndsAt)
	expiredRun, err := h.store.CreateDemo(ctx, "expired", h.clock.Now())
	must(t, err)
	release := h.holdLock(t, func(tx app.Tx) error { _, err := tx.Draw(ctx, h.run.DrawID, true); return err })
	finish := startTimeoutTick(t, h)
	h.waitForLock(t, "FROM draws WHERE id=$1 FOR UPDATE")
	if err := finish(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scheduler lost the blocked draw timeout: %v", err)
	}
	blocked, err := h.store.Draw(ctx, h.run.DrawID)
	must(t, err)
	completed, err := h.store.Draw(ctx, independent.DrawID)
	must(t, err)
	expired, err := h.store.Offer(ctx, expiredRun.OfferID)
	must(t, err)
	if blocked.State != domain.DrawOpen || completed.State != domain.DrawDrawn || expired.Status != "expired" || len(h.offers(t, expiredRun.DrawID)) != 2 {
		t.Fatalf("one draw blocked other scheduler work: blocked=%+v independent=%+v expired=%+v", blocked, completed, expired)
	}
	release()
	must(t, h.service.Tick(ctx))
	recovered, err := h.store.Draw(ctx, h.run.DrawID)
	must(t, err)
	if recovered.State != domain.DrawDrawn {
		t.Fatalf("released draw was not recovered: %+v", recovered)
	}
}

func TestSchedulerOfferTimeoutStillExpiresTheNextOffer(t *testing.T) {
	h := timeoutHarness(t, "expired")
	ctx := context.Background()
	blocked, err := h.store.Offer(ctx, h.run.OfferID)
	must(t, err)
	independentRun, err := h.store.CreateDemo(ctx, "expired", h.clock.Now())
	must(t, err)
	must(t, h.store.Within(ctx, func(tx app.Tx) error {
		offer, err := tx.Offer(ctx, independentRun.OfferID)
		if err != nil {
			return err
		}
		deadline := blocked.Deadline.Add(time.Minute)
		offer.Deadline = &deadline
		return tx.SaveOffer(ctx, offer)
	}))
	release := h.holdLock(t, func(tx app.Tx) error { return tx.LockSlot(ctx, blocked.SlotID) })
	finish := startTimeoutTick(t, h)
	h.waitForLock(t, "FROM slots")
	if err := finish(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scheduler lost the blocked offer timeout: %v", err)
	}
	unchanged, err := h.store.Offer(ctx, blocked.ID)
	must(t, err)
	expired, err := h.store.Offer(ctx, independentRun.OfferID)
	must(t, err)
	if unchanged.Status != "notified" || expired.Status != "expired" || len(h.offers(t, h.run.DrawID)) != 1 || len(h.offers(t, independentRun.DrawID)) != 2 {
		t.Fatalf("one offer blocked the next expiration: blocked=%+v independent=%+v", unchanged, expired)
	}
	release()
	must(t, h.service.Tick(ctx))
	recovered, err := h.store.Offer(ctx, blocked.ID)
	must(t, err)
	if recovered.Status != "expired" || len(h.offers(t, h.run.DrawID)) != 2 {
		t.Fatalf("released offer was not recovered exactly once: %+v", recovered)
	}
}
