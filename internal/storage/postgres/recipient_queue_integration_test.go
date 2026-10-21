//go:build integration

package postgres

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
)

func enqueueMessages(t *testing.T, store *Store, now time.Time, messages ...domain.Message) {
	t.Helper()
	ctx := context.Background()
	requireNoError(t, store.Within(ctx, func(tx app.Tx) error {
		for _, message := range messages {
			if err := tx.Enqueue(ctx, message, message.ID, now); err != nil {
				return err
			}
		}
		return nil
	}))
}

func TestOutboxParallelClaimsSerializeRecipientsAcrossCampaigns(t *testing.T) {
	s := testStore(t)
	run := fixture(t, s, "standard")
	other := fixture(t, s, "standard")
	ctx := context.Background()
	first := domain.Message{ID: "first", CampaignID: run.CampaignID, UserID: run.UserIDs[0], Text: "first"}
	second := domain.Message{ID: "second", CampaignID: other.CampaignID, UserID: first.UserID, Text: "second campaign"}
	independent := domain.Message{ID: "independent", CampaignID: run.CampaignID, UserID: run.UserIDs[1], Text: "independent"}
	enqueueMessages(t, s, run.Now, first, second, independent)
	start := make(chan struct{})
	claimed := make(chan *domain.Message, 16)
	var workers sync.WaitGroup
	for range cap(claimed) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			message, err := s.TakeMessage(ctx, run.Now)
			if err != nil {
				t.Error(err)
				return
			}
			if message != nil {
				claimed <- message
			}
		}()
	}
	close(start)
	workers.Wait()
	close(claimed)
	seen := map[string]bool{}
	for message := range claimed {
		if seen[message.ID] || message.Attempts != 1 {
			t.Fatalf("message claimed more than once: %+v", message)
		}
		seen[message.ID] = true
	}
	if len(seen) != 2 || !seen[first.ID] || !seen[independent.ID] {
		t.Fatalf("parallel claims bypassed a recipient head or blocked another recipient: %v", seen)
	}
	requireNoError(t, s.FinishMessage(ctx, independent.ID))
	message, err := s.TakeMessage(ctx, run.Now.Add(30*time.Second-time.Nanosecond))
	requireNoError(t, err)
	if message != nil {
		t.Fatalf("active recipient lease was bypassed: %+v", message)
	}
	message, err = s.TakeMessage(ctx, run.Now.Add(30*time.Second))
	requireNoError(t, err)
	if message == nil || message.ID != first.ID || message.Attempts != 2 {
		t.Fatalf("expired recipient head was not recovered first: %+v", message)
	}
	requireNoError(t, s.FinishMessage(ctx, message.ID))
	message, err = s.TakeMessage(ctx, run.Now.Add(30*time.Second))
	requireNoError(t, err)
	if message == nil || message.ID != second.ID || message.Attempts != 1 {
		t.Fatalf("next campaign message did not follow the completed head: %+v", message)
	}
	requireNoError(t, s.FinishMessage(ctx, message.ID))
	message, err = s.TakeMessage(ctx, run.Now.Add(time.Hour))
	requireNoError(t, err)
	if message != nil {
		t.Fatalf("completed message was taken again: %+v", message)
	}
}

func TestOutboxRetryCooldownBlocksOnlyItsRecipient(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal=%v", terminal), func(t *testing.T) {
			s := testStore(t)
			run := fixture(t, s, "standard")
			ctx := context.Background()
			first := domain.Message{ID: "first", CampaignID: run.CampaignID, UserID: run.UserIDs[0], Text: "first"}
			second := domain.Message{ID: "second", CampaignID: run.CampaignID, UserID: first.UserID, Text: "second"}
			independent := domain.Message{ID: "independent", CampaignID: run.CampaignID, UserID: run.UserIDs[1], Text: "independent"}
			enqueueMessages(t, s, run.Now, first, second, independent)
			message, err := s.TakeMessage(ctx, run.Now)
			requireNoError(t, err)
			if message == nil || message.ID != first.ID {
				t.Fatalf("wrong queue head: %+v", message)
			}
			next := run.Now.Add(5 * time.Minute)
			requireNoError(t, s.RetryMessage(ctx, first.ID, "rate limited", next, terminal))
			message, err = s.TakeMessage(ctx, run.Now)
			requireNoError(t, err)
			want := independent.ID
			if terminal {
				want = second.ID
			}
			if message == nil || message.ID != want {
				t.Fatalf("recipient state blocked or bypassed the wrong message: got=%+v want=%s", message, want)
			}
			requireNoError(t, s.FinishMessage(ctx, message.ID))
			if terminal {
				message, err = s.TakeMessage(ctx, run.Now)
				requireNoError(t, err)
				if message == nil || message.ID != independent.ID {
					t.Fatalf("terminal head stalled an independent recipient: %+v", message)
				}
				requireNoError(t, s.FinishMessage(ctx, message.ID))
			} else {
				message, err = s.TakeMessage(ctx, next.Add(-time.Nanosecond))
				requireNoError(t, err)
				if message != nil {
					t.Fatalf("recipient cooldown was bypassed: %+v", message)
				}
				message, err = s.TakeMessage(ctx, next)
				requireNoError(t, err)
				if message == nil || message.ID != first.ID || message.Attempts != 2 {
					t.Fatalf("retry was not eligible at its cooldown boundary: %+v", message)
				}
				requireNoError(t, s.FinishMessage(ctx, message.ID))
				message, err = s.TakeMessage(ctx, next)
				requireNoError(t, err)
				if message == nil || message.ID != second.ID || message.Attempts != 1 {
					t.Fatalf("recipient order lost after retry: %+v", message)
				}
				requireNoError(t, s.FinishMessage(ctx, message.ID))
			}
			message, err = s.TakeMessage(ctx, next.Add(time.Hour))
			requireNoError(t, err)
			if message != nil {
				t.Fatalf("terminal or delivered message was replayed: %+v", message)
			}
		})
	}
}

func TestOutboxRecipientIndexMigrationPreservesQueuedMessages(t *testing.T) {
	s := testStore(t)
	run := fixture(t, s, "standard")
	ctx := context.Background()
	enqueueMessages(t, s, run.Now,
		domain.Message{ID: "first", CampaignID: run.CampaignID, UserID: run.UserIDs[0], Text: "first"},
		domain.Message{ID: "second", CampaignID: run.CampaignID, UserID: run.UserIDs[0], Text: "second"},
	)
	_, err := s.pool.Exec(ctx, "DROP INDEX outbox_recipient_pending_idx")
	requireNoError(t, err)
	_, err = s.pool.Exec(ctx, "DELETE FROM schema_migrations WHERE version='migrations/002_outbox_recipient_order.sql'")
	requireNoError(t, err)
	requireNoError(t, s.Migrate(ctx))
	requireNoError(t, s.Migrate(ctx))
	messages, err := s.Messages(ctx, run.CampaignID, "")
	requireNoError(t, err)
	if len(messages) != 2 || messages[0].ID != "first" || messages[1].ID != "second" || messages[0].State != "pending" || messages[1].State != "pending" {
		t.Fatalf("index migration changed the existing queue: %+v", messages)
	}
	var exists bool
	requireNoError(t, s.pool.QueryRow(ctx, "SELECT to_regclass('outbox_recipient_pending_idx') IS NOT NULL").Scan(&exists))
	if !exists {
		t.Fatal("recipient index was not installed on the populated queue")
	}
}

func TestOutboxAdministrativeRetryWaitsForActiveRecipientDelivery(t *testing.T) {
	for _, recoverLease := range []bool{false, true} {
		t.Run(fmt.Sprintf("recover lease=%v", recoverLease), func(t *testing.T) {
			s := testStore(t)
			run := fixture(t, s, "expired")
			ctx := context.Background()
			offer, err := s.Offer(ctx, run.OfferID)
			requireNoError(t, err)
			first := domain.Message{ID: "first", CampaignID: run.CampaignID, UserID: offer.UserID, Text: "prize", OfferID: offer.ID}
			second := domain.Message{ID: "second", CampaignID: run.CampaignID, UserID: offer.UserID, Text: "later reply"}
			independent := domain.Message{ID: "independent", CampaignID: run.CampaignID, UserID: run.UserIDs[1], Text: "independent"}
			enqueueMessages(t, s, run.Now, first, second, independent)
			message, err := s.TakeMessage(ctx, run.Now)
			requireNoError(t, err)
			if message == nil || message.ID != first.ID {
				t.Fatalf("wrong original delivery: %+v", message)
			}
			requireNoError(t, s.RetryMessage(ctx, first.ID, "delivery uncertain", run.Now, true))
			message, err = s.TakeMessage(ctx, run.Now)
			requireNoError(t, err)
			if message == nil || message.ID != second.ID {
				t.Fatalf("terminal delivery blocked the later reply: %+v", message)
			}
			requireNoError(t, s.Within(ctx, func(tx app.Tx) error { return tx.RetryOfferMessage(ctx, offer.ID, run.Now) }))
			message, err = s.TakeMessage(ctx, run.Now)
			requireNoError(t, err)
			if message == nil || message.ID != independent.ID {
				t.Fatalf("administrative retry overlapped the active recipient delivery: %+v", message)
			}
			requireNoError(t, s.FinishMessage(ctx, message.ID))
			message, err = s.TakeMessage(ctx, run.Now.Add(30*time.Second-time.Nanosecond))
			requireNoError(t, err)
			if message != nil {
				t.Fatalf("administrative retry bypassed an active recipient lease: %+v", message)
			}
			now := run.Now
			if recoverLease {
				now = run.Now.Add(30 * time.Second)
			} else {
				requireNoError(t, s.FinishMessage(ctx, second.ID))
			}
			message, err = s.TakeMessage(ctx, now)
			requireNoError(t, err)
			if message == nil || message.ID != first.ID || message.Attempts != 1 {
				t.Fatalf("administrative retry did not resume with reset attempts: %+v", message)
			}
			requireNoError(t, s.FinishMessage(ctx, message.ID))
			if recoverLease {
				message, err = s.TakeMessage(ctx, now)
				requireNoError(t, err)
				if message == nil || message.ID != second.ID || message.Attempts != 2 {
					t.Fatalf("later expired lease did not recover after the retried head: %+v", message)
				}
				requireNoError(t, s.FinishMessage(ctx, message.ID))
			}
			message, err = s.TakeMessage(ctx, now.Add(time.Hour))
			requireNoError(t, err)
			if message != nil {
				t.Fatalf("completed administrative retry was replayed: %+v", message)
			}
		})
	}
}
