//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/app"
	"promo/internal/domain"
)

func administrativeRetryQueue(t *testing.T) (*Store, domain.DemoRun, domain.Offer) {
	t.Helper()
	s := testStore(t)
	run := fixture(t, s, "expired")
	ctx := context.Background()
	offer, err := s.Offer(ctx, run.OfferID)
	requireNoError(t, err)
	enqueueMessages(t, s, run.Now,
		domain.Message{ID: "first", CampaignID: run.CampaignID, UserID: offer.UserID, Text: "prize", OfferID: offer.ID},
		domain.Message{ID: "second", CampaignID: run.CampaignID, UserID: offer.UserID, Text: "later reply"},
		domain.Message{ID: "independent", CampaignID: run.CampaignID, UserID: run.UserIDs[1], Text: "independent"},
	)
	message, err := s.TakeMessage(ctx, run.Now)
	requireNoError(t, err)
	if message == nil || message.ID != "first" {
		t.Fatalf("unexpected original prize notification: %+v", message)
	}
	requireNoError(t, s.RetryMessage(ctx, message.ID, "delivery uncertain", run.Now, true))
	return s, run, offer
}

func TestOutboxAdministrativeRetryTransactionSerializesRecipientClaims(t *testing.T) {
	s, run, offer := administrativeRetryQueue(t)
	ctx := context.Background()
	retry, err := s.pool.Begin(ctx)
	requireNoError(t, err)
	defer retry.Rollback(ctx)
	requireNoError(t, (&transaction{tx: retry}).RetryOfferMessage(ctx, offer.ID, run.Now))
	message, err := s.TakeMessage(ctx, run.Now)
	requireNoError(t, err)
	if message == nil || message.ID != "independent" {
		t.Fatalf("uncommitted administrative retry blocked or overlapped the wrong recipient: %+v", message)
	}
	requireNoError(t, s.FinishMessage(ctx, message.ID))
	message, err = s.TakeMessage(ctx, run.Now)
	requireNoError(t, err)
	if message != nil {
		t.Fatalf("delivery bypassed the recipient's administrative retry transaction: %+v", message)
	}
	requireNoError(t, retry.Commit(ctx))
	for _, id := range []string{"first", "second"} {
		message, err = s.TakeMessage(ctx, run.Now)
		requireNoError(t, err)
		if message == nil || message.ID != id || message.Attempts != 1 {
			t.Fatalf("administrative retry changed recipient order: got=%+v want=%s", message, id)
		}
		requireNoError(t, s.FinishMessage(ctx, message.ID))
	}
}

func TestOutboxConcurrentAdministrativeRetryCannotResurrectAnActiveRecipientHead(t *testing.T) {
	s, run, offer := administrativeRetryQueue(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gate, err := s.pool.Acquire(ctx)
	requireNoError(t, err)
	defer gate.Release()
	const gateKey = `hashtextextended(current_schema() || ':outbox-concurrency-test', 0)`
	_, err = gate.Exec(ctx, "SELECT pg_advisory_lock("+gateKey+")")
	requireNoError(t, err)
	var once sync.Once
	release := func() {
		once.Do(func() {
			_, err := gate.Exec(context.Background(), "SELECT pg_advisory_unlock("+gateKey+")")
			requireNoError(t, err)
		})
	}
	defer release()
	_, err = s.pool.Exec(ctx, `CREATE FUNCTION pause_second_delivery() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
   IF NEW.id = 'second' THEN
     PERFORM pg_advisory_xact_lock(hashtextextended(current_schema() || ':outbox-concurrency-test', 0));
   END IF;
   RETURN NEW;
 END $$;
 CREATE TRIGGER pause_second_delivery BEFORE UPDATE OF state ON outbox
 FOR EACH ROW EXECUTE FUNCTION pause_second_delivery()`)
	requireNoError(t, err)
	type result struct {
		message *domain.Message
		err     error
	}
	taken := make(chan result, 1)
	go func() {
		message, err := s.TakeMessage(ctx, run.Now)
		taken <- result{message, err}
	}()
	deliveryPID := waitForQueueBlocker(t, s, gate.Conn().PgConn().PID())
	retried := make(chan error, 1)
	go func() {
		retried <- s.Within(ctx, func(tx app.Tx) error { return tx.RetryOfferMessage(ctx, offer.ID, run.Now) })
	}()
	waitForQueueBlocker(t, s, deliveryPID)
	release()
	var delivery result
	select {
	case delivery = <-taken:
	case <-ctx.Done():
		t.Fatal("recipient delivery did not finish after releasing the database gate")
	}
	requireNoError(t, delivery.err)
	if delivery.message == nil || delivery.message.ID != "second" {
		t.Fatalf("unexpected concurrent delivery: %+v", delivery.message)
	}
	select {
	case err = <-retried:
		requireNoError(t, err)
	case <-ctx.Done():
		t.Fatal("administrative retry did not finish after the recipient claim committed")
	}
	message, err := s.TakeMessage(ctx, run.Now)
	requireNoError(t, err)
	if message == nil || message.ID != "independent" {
		t.Fatalf("resurrected head overlapped the active recipient delivery: %+v", message)
	}
	requireNoError(t, s.FinishMessage(ctx, message.ID))
	var active int
	requireNoError(t, s.pool.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE user_id=$1 AND state='processing' AND lease_until>$2", offer.UserID, run.Now).Scan(&active))
	if active != 1 {
		t.Fatalf("concurrent retry produced %d active recipient leases", active)
	}
	requireNoError(t, s.FinishMessage(ctx, "second"))
	message, err = s.TakeMessage(ctx, run.Now)
	requireNoError(t, err)
	if message == nil || message.ID != "first" || message.Attempts != 1 {
		t.Fatalf("administrative retry did not resume after the active delivery: %+v", message)
	}
	requireNoError(t, s.FinishMessage(ctx, message.ID))
}

func waitForQueueBlocker(t *testing.T, s *Store, blocker uint32) uint32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pid uint32
		err := s.pool.QueryRow(ctx, "SELECT pid FROM pg_stat_activity WHERE $1::integer=ANY(pg_blocking_pids(pid)) LIMIT 1", int32(blocker)).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("inspect concurrent recipient lock: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("concurrent operation did not wait for its recipient lock")
		case <-ticker.C:
		}
	}
}
