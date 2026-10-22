//go:build integration

package app_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/app"
	"promo/internal/domain"
	"promo/internal/platform/demo"
	"promo/internal/storage/postgres"
	"promo/internal/testdb"
)

type testClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (c *testClock) Now() time.Time { c.mu.RLock(); defer c.mu.RUnlock(); return c.now }

func (c *testClock) Set(now time.Time) { c.mu.Lock(); defer c.mu.Unlock(); c.now = now }

type firstCandidate struct{}

func (firstCandidate) Intn(int64) (int64, error) { return 0, nil }

type harness struct {
	store     *postgres.Store
	service   *app.Service
	messenger *demo.Messenger
	clock     *testClock
	run       domain.DemoRun
	observer  *pgx.Conn
	name      string
}

func setup(t *testing.T, scenario string) *harness {
	t.Helper()
	return setupAt(t, scenario, time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC))
}

func setupAt(t *testing.T, scenario string, now time.Time) *harness {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Fatal("integration tests require TEST_DATABASE_URL")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, raw)
	must(t, err)
	schema := "app_test_" + strings.ToLower(rand.Text())
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	must(t, err)
	parsed, err := url.Parse(raw)
	must(t, err)
	query := parsed.Query()
	query.Set("search_path", schema)
	query.Set("application_name", schema)
	parsed.RawQuery = query.Encode()
	store, err := postgres.New(ctx, parsed.String())
	must(t, err)
	t.Cleanup(func() {
		store.Close()
		_, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		if err != nil {
			t.Error(err)
		}
		admin.Close(ctx)
	})
	must(t, store.Migrate(ctx))
	must(t, testdb.IsolateDemoUsers(ctx, admin, schema))
	clock := &testClock{now: now}
	run, err := store.CreateDemo(ctx, scenario, clock.Now())
	must(t, err)
	messenger := demo.New()
	return &harness{store: store, service: app.New(store, messenger, clock, firstCandidate{}), messenger: messenger, clock: clock, run: run, observer: admin, name: schema}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (h *harness) command(action string) domain.Command {
	return domain.Command{ID: rand.Text(), CampaignID: h.run.CampaignID, UserID: h.run.UserIDs[0], Action: action}
}

func (h *harness) execute(t *testing.T, c domain.Command) domain.Message {
	t.Helper()
	reply, err := h.service.Execute(context.Background(), c)
	must(t, err)
	return reply
}

func (h *harness) offers(t *testing.T, draw string) []domain.Offer {
	t.Helper()
	var offers []domain.Offer
	must(t, h.store.Within(context.Background(), func(tx app.Tx) error { var err error; offers, err = tx.Offers(context.Background(), draw); return err }))
	return offers
}

func concurrently(t *testing.T, n int, fn func(int) error) {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := fn(i); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
}

func TestIndependentFixturesDoNotShareRecipientLocks(t *testing.T) {
	first := setup(t, "standard")
	second := setup(t, "standard")
	ctx := context.Background()
	lock, err := first.observer.Begin(ctx)
	must(t, err)
	defer lock.Rollback(ctx)
	_, err = lock.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('promo.outbox:' || $1::bigint::text, 0))", first.run.UserIDs[0])
	must(t, err)
	second.execute(t, second.command("start"))
	message, err := second.store.TakeMessage(ctx, second.clock.Now())
	must(t, err)
	if message == nil || message.UserID != second.run.UserIDs[0] {
		t.Fatalf("another fixture's recipient lock blocked delivery: %+v", message)
	}
}

func TestApplicationConcurrentPromoAndRegistrationGrantOneChance(t *testing.T) {
	h := setup(t, "standard")
	ctx := context.Background()
	concurrently(t, 60, func(i int) error {
		action := "promo"
		if i%2 == 1 {
			action = "check"
		}
		_, err := h.service.Execute(ctx, h.command(action))
		return err
	})
	active, err := h.store.Activities(ctx, h.run.CampaignID, h.run.UserIDs[0])
	must(t, err)
	if len(active) != 1 {
		t.Fatalf("chances=%d", len(active))
	}
	stats, err := h.store.Stats(ctx, h.run.CampaignID)
	must(t, err)
	if stats.PromoIssued != 1 || stats.Entries[h.run.DrawID] != 1 || stats.PromoRequests != 30 {
		t.Fatalf("stats=%+v", stats)
	}
}

func TestApplicationDurableDuplicateCommandMutatesOnce(t *testing.T) {
	h := setup(t, "standard")
	ctx := context.Background()
	command := h.command("promo")
	inserted, err := h.store.PutInbox(ctx, command, "duplicate-delivery")
	must(t, err)
	if !inserted {
		t.Fatal("first command was rejected")
	}
	concurrently(t, 20, func(int) error { _, err := h.service.Execute(ctx, command); return err })
	stats, err := h.store.Stats(ctx, h.run.CampaignID)
	must(t, err)
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if stats.PromoRequests != 1 || stats.PromoIssued != 1 || len(messages) != 1 {
		t.Fatalf("stats=%+v messages=%d", stats, len(messages))
	}
}

func TestApplicationSubscriptionFailuresNeverRegister(t *testing.T) {
	h := setup(t, "standard")
	ctx := context.Background()
	day, err := h.store.CurrentDay(ctx, h.run.CampaignID, h.clock.Now())
	must(t, err)
	for _, status := range []string{"no", "error"} {
		h.messenger.SetSubscription(day.ChannelID, h.run.UserIDs[0], status)
		reply := h.execute(t, h.command("check"))
		if reply.Text == "" {
			t.Fatal("failure has no explanation")
		}
		stats, err := h.store.Stats(ctx, h.run.CampaignID)
		must(t, err)
		if stats.Entries[h.run.DrawID] != 0 {
			t.Fatal("failed subscription registered a user")
		}
		active, err := h.store.Activities(ctx, h.run.CampaignID, h.run.UserIDs[0])
		must(t, err)
		if len(active) != 0 {
			t.Fatal("failed subscription granted a chance")
		}
	}
	h.messenger.SetSubscription(day.ChannelID, h.run.UserIDs[0], "yes")
	h.execute(t, h.command("check"))
	stats, err := h.store.Stats(ctx, h.run.CampaignID)
	must(t, err)
	if stats.Entries[h.run.DrawID] != 1 {
		t.Fatal("successful subscription did not register")
	}
}

func TestApplicationEmptyPoolStillAllowsParticipation(t *testing.T) {
	h := setup(t, "exhausted")
	ctx := context.Background()
	reply := h.execute(t, h.command("promo"))
	if !strings.Contains(reply.Text, "закончились") {
		t.Fatalf("unexpected reply: %s", reply.Text)
	}
	active, err := h.store.Activities(ctx, h.run.CampaignID, h.run.UserIDs[0])
	must(t, err)
	if len(active) != 0 {
		t.Fatal("empty pool granted a chance")
	}
	h.execute(t, h.command("check"))
	active, err = h.store.Activities(ctx, h.run.CampaignID, h.run.UserIDs[0])
	must(t, err)
	if len(active) != 1 {
		t.Fatal("empty pool blocked registration")
	}
}

func TestApplicationConcurrentDrawAndApprovalAreIdempotent(t *testing.T) {
	h := setup(t, "drawable")
	ctx := context.Background()
	concurrently(t, 40, func(int) error {
		_, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "test-admin")
		return err
	})
	drawn, err := h.store.Draw(ctx, h.run.DrawID)
	must(t, err)
	if drawn.State != domain.DrawDrawn || len(drawn.Candidates) != 8 || len(drawn.Selection.Winners) != 1 || len(drawn.Selection.Reserve) != 3 {
		t.Fatalf("draw=%+v", drawn)
	}
	before, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if len(before) != 0 {
		t.Fatal("winners notified before approval")
	}
	concurrently(t, 40, func(int) error {
		_, err := h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "test-admin")
		return err
	})
	offers := h.offers(t, h.run.DrawID)
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if len(offers) != 1 || len(messages) != 1 || offers[0].Status != "pending" || offers[0].Deadline != nil {
		t.Fatalf("offers=%+v messages=%+v", offers, messages)
	}
	restarted := app.New(h.store, h.messenger, h.clock, firstCandidate{})
	again, err := restarted.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "test-admin")
	must(t, err)
	if !reflect.DeepEqual(drawn.Selection, again.Selection) {
		t.Fatal("restart changed the saved selection")
	}
}

func TestApplicationDeliveryStartsDeadlineOnceAndClaimSurvivesExpiry(t *testing.T) {
	h := setup(t, "drawable")
	ctx := context.Background()
	_, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "test-admin")
	must(t, err)
	_, err = h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "test-admin")
	must(t, err)
	offer := h.offers(t, h.run.DrawID)[0]
	must(t, h.service.MarkDelivered(ctx, offer.ID))
	notified, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if notified.Status != "notified" || notified.Deadline == nil {
		t.Fatal("delivery did not activate deadline")
	}
	h.clock.Set(h.clock.Now().Add(time.Hour))
	must(t, h.service.MarkDelivered(ctx, offer.ID))
	same, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if !same.Deadline.Equal(*notified.Deadline) {
		t.Fatal("duplicate delivery extended deadline")
	}
	command := h.command("claim")
	command.UserID = offer.UserID
	command.OfferID = offer.ID
	command.Name = "Иван Петров"
	command.Consent = true
	h.execute(t, command)
	claimed, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if claimed.Status != "claimed" || claimed.ConsentAt == nil || claimed.Name != "Иван Петров" {
		t.Fatalf("offer=%+v", claimed)
	}
	h.clock.Set(notified.Deadline.Add(time.Second))
	must(t, h.service.ExpireOffer(ctx, offer.ID))
	offers := h.offers(t, h.run.DrawID)
	if len(offers) != 1 || offers[0].Status != "claimed" {
		t.Fatal("claimed prize passed to reserve")
	}
}

func TestApplicationExpiredPrizePassesToOneReserveUnderConcurrency(t *testing.T) {
	h := setup(t, "expired")
	ctx := context.Background()
	old, err := h.store.Offer(ctx, h.run.OfferID)
	must(t, err)
	concurrently(t, 20, func(int) error { return h.service.ExpireOffer(ctx, old.ID) })
	offers := h.offers(t, h.run.DrawID)
	if len(offers) != 2 {
		t.Fatalf("offers=%+v", offers)
	}
	var next domain.Offer
	for _, offer := range offers {
		if offer.ID == old.ID {
			if offer.Status != "expired" {
				t.Fatal("old offer still active")
			}
		} else {
			next = offer
		}
	}
	if next.SlotID != old.SlotID || next.UserID != h.run.UserIDs[1] || next.Status != "pending" {
		t.Fatalf("reserve=%+v", next)
	}
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if len(messages) != 1 {
		t.Fatal("reserve received duplicate notification jobs")
	}
	must(t, h.service.MarkDelivered(ctx, next.ID))
	notified, err := h.store.Offer(ctx, next.ID)
	must(t, err)
	h.clock.Set(notified.Deadline.Add(time.Second))
	must(t, h.service.ExpireOffer(ctx, next.ID))
	offers = h.offers(t, h.run.DrawID)
	if len(offers) != 3 {
		t.Fatal("second reserve was not offered the prize")
	}
	for _, offer := range offers {
		if offer.Status == "pending" && offer.UserID != h.run.UserIDs[2] {
			t.Fatal("reserve order changed")
		}
	}
}

func TestApplicationClaimAtDeadlineCannotOvertakeReserve(t *testing.T) {
	h := setup(t, "expired")
	ctx := context.Background()
	old, err := h.store.Offer(ctx, h.run.OfferID)
	must(t, err)
	h.clock.Set(*old.Deadline)
	command := h.command("claim")
	command.UserID = old.UserID
	command.OfferID = old.ID
	command.Name = "Иван Петров"
	command.Consent = true
	concurrently(t, 20, func(i int) error {
		if i%2 == 0 {
			return h.service.ExpireOffer(ctx, old.ID)
		}
		c := command
		c.ID = rand.Text()
		_, err := h.service.Execute(ctx, c)
		return err
	})
	offers := h.offers(t, h.run.DrawID)
	if len(offers) != 2 {
		t.Fatalf("offers=%+v", offers)
	}
	for _, offer := range offers {
		if offer.ID == old.ID && offer.Status != "expired" {
			t.Fatal("claim at deadline succeeded")
		}
	}
}

func TestApplicationFinalFreezesWeightsAndAllowsPrizeClaimAfterClosure(t *testing.T) {
	h := setup(t, "final")
	ctx := context.Background()
	d, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.FinalDrawID, "test-admin")
	must(t, err)
	if len(d.Candidates) != 8 || len(d.Selection.Winners) != 5 {
		t.Fatalf("draw=%+v", d)
	}
	for i, c := range d.Candidates {
		if c.Weight != i+1 {
			t.Fatalf("weight=%d expected=%d", c.Weight, i+1)
		}
	}
	c, err := h.store.Campaign(ctx, h.run.CampaignID)
	must(t, err)
	if c.Status != "closed" {
		t.Fatal("final did not close campaign")
	}
	_, err = h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.FinalDrawID, "test-admin")
	must(t, err)
	offers := h.offers(t, h.run.FinalDrawID)
	if len(offers) != 5 {
		t.Fatalf("offer count=%d", len(offers))
	}
	offer := offers[0]
	must(t, h.service.MarkDelivered(ctx, offer.ID))
	command := h.command("claim")
	command.UserID = offer.UserID
	command.OfferID = offer.ID
	command.Name = "Иван Петров"
	command.Consent = true
	h.execute(t, command)
	claimed, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if claimed.Status != "claimed" {
		t.Fatal("campaign closure blocked prize claim")
	}
}

// holdLock waits until the first transaction owns the actual database lock.
// Tests release it only after PostgreSQL reports the competing query waiting.
func (h *harness) holdLock(t *testing.T, lock func(app.Tx) error) func() {
	t.Helper()
	locked := make(chan struct{})
	unlock := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- h.store.Within(context.Background(), func(tx app.Tx) error {
			if err := lock(tx); err != nil {
				return err
			}
			close(locked)
			<-unlock
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-done:
		t.Fatalf("acquire test lock: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out acquiring test lock")
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			close(unlock)
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)
	return release
}

func (h *harness) waitForLock(t *testing.T, queryFragment string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := h.observer.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE $2)`, h.name, "%"+queryFragment+"%").Scan(&waiting)
		must(t, err)
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("competing query never waited for the expected lock")
		case <-ticker.C:
		}
	}
}

func TestApplicationWaitingForPromoPoolRechecksDayBoundary(t *testing.T) {
	h := setup(t, "standard")
	ctx := context.Background()
	day, err := h.store.CurrentDay(ctx, h.run.CampaignID, h.clock.Now())
	must(t, err)
	release := h.holdLock(t, func(tx app.Tx) error { return tx.LockPool(ctx, day.ID) })
	done := make(chan error, 1)
	go func() { _, err := h.service.Execute(ctx, h.command("promo")); done <- err }()
	h.waitForLock(t, "FROM promo_pools")
	h.clock.Set(day.EndsAt)
	release()
	must(t, <-done)
	stats, err := h.store.Stats(ctx, h.run.CampaignID)
	must(t, err)
	if stats.PromoIssued != 0 {
		t.Fatal("a blocked request received yesterday's code after midnight")
	}
	active, err := h.store.Activities(ctx, h.run.CampaignID, h.run.UserIDs[0])
	must(t, err)
	if len(active) != 0 {
		t.Fatal("stale day granted a chance")
	}
}

func TestApplicationWaitingForPrizeSlotRechecksDeadline(t *testing.T) {
	h := setup(t, "drawable")
	ctx := context.Background()
	_, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "test-admin")
	must(t, err)
	_, err = h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "test-admin")
	must(t, err)
	offer := h.offers(t, h.run.DrawID)[0]
	must(t, h.service.MarkDelivered(ctx, offer.ID))
	offer, err = h.store.Offer(ctx, offer.ID)
	must(t, err)
	release := h.holdLock(t, func(tx app.Tx) error { return tx.LockSlot(ctx, offer.SlotID) })
	command := h.command("claim")
	command.UserID = offer.UserID
	command.OfferID = offer.ID
	command.Name = "Иван Петров"
	command.Consent = true
	done := make(chan error, 1)
	go func() { _, err := h.service.Execute(ctx, command); done <- err }()
	h.waitForLock(t, "FROM slots")
	h.clock.Set(*offer.Deadline)
	release()
	must(t, <-done)
	after, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if after.Status == "claimed" {
		t.Fatal("request used the time captured before waiting for the prize lock")
	}
	must(t, h.service.ExpireOffer(ctx, offer.ID))
	if len(h.offers(t, h.run.DrawID)) != 2 {
		t.Fatal("expired offer did not pass to reserve")
	}
}

func TestApplicationUncertainDeliveryWaitsForAdministrator(t *testing.T) {
	h := setup(t, "drawable")
	ctx := context.Background()
	_, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "test-admin")
	must(t, err)
	_, err = h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "test-admin")
	must(t, err)
	offer := h.offers(t, h.run.DrawID)[0]
	message, err := h.store.TakeMessage(ctx, h.clock.Now())
	must(t, err)
	if message == nil {
		t.Fatal("prize notification was not queued")
	}
	must(t, h.store.RetryMessage(ctx, message.ID, "delivery uncertain", h.clock.Now(), true))
	failed, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if failed.Status != "pending" || failed.Deadline != nil || failed.DeliveryState != "failed" || failed.DeliveryError != "delivery uncertain" {
		t.Fatalf("offer=%+v", failed)
	}
	h.clock.Set(h.clock.Now().Add(7 * 24 * time.Hour))
	must(t, h.service.ExpireOffer(ctx, offer.ID))
	if len(h.offers(t, h.run.DrawID)) != 1 {
		t.Fatal("unconfirmed delivery lost the participant's prize")
	}
	must(t, h.service.RetryDelivery(ctx, offer.ID, "test-admin"))
	retried, err := h.store.TakeMessage(ctx, h.clock.Now())
	must(t, err)
	if retried == nil || retried.ID != message.ID || retried.Attempts != 1 {
		t.Fatalf("retried=%+v", retried)
	}
	must(t, h.service.ConfirmDelivery(ctx, offer.ID, "test-admin"))
	confirmed, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if confirmed.Status != "notified" || confirmed.Deadline == nil {
		t.Fatal("administrator could not confirm delivery")
	}
	if err = h.service.RetryDelivery(ctx, offer.ID, "test-admin"); err == nil {
		t.Fatal("confirmed prize notification was reset")
	}
}

// writeGate pauses a successful write before its surrounding transaction commits.
// The competing closer must wait for that real transaction's row locks.
type writeGate struct {
	kind        string
	reached     chan struct{}
	release     chan struct{}
	pauseOnce   sync.Once
	releaseOnce sync.Once
}

func newWriteGate(t *testing.T, kind string) *writeGate {
	t.Helper()
	g := &writeGate{kind: kind, reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(g.resume)
	return g
}

func (g *writeGate) pause(kind string) {
	if g.kind == kind {
		g.pauseOnce.Do(func() { close(g.reached); <-g.release })
	}
}

func (g *writeGate) resume() { g.releaseOnce.Do(func() { close(g.release) }) }

func (g *writeGate) wait(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("business mutation did not reach the transaction gate")
	}
}

type gatedRepository struct {
	app.Repository
	gate *writeGate
}

func (r gatedRepository) Within(ctx context.Context, fn func(app.Tx) error) error {
	return r.Repository.Within(ctx, func(tx app.Tx) error { return fn(gatedTransaction{Tx: tx, gate: r.gate}) })
}

type gatedTransaction struct {
	app.Tx
	gate *writeGate
}

func (tx gatedTransaction) AddEntry(ctx context.Context, draw string, user int64, at time.Time) error {
	if err := tx.Tx.AddEntry(ctx, draw, user, at); err != nil {
		return err
	}
	tx.gate.pause("entry")
	return nil
}

func (tx gatedTransaction) AddActivity(ctx context.Context, campaign string, user int64, day domain.Day, source string, at time.Time) error {
	if err := tx.Tx.AddActivity(ctx, campaign, user, day, source, at); err != nil {
		return err
	}
	tx.gate.pause("activity")
	return nil
}

func TestApplicationAcceptedRegistrationIsInDailyClosingSnapshot(t *testing.T) {
	h := setup(t, "standard")
	ctx := context.Background()
	day, err := h.store.CurrentDay(ctx, h.run.CampaignID, h.clock.Now())
	must(t, err)
	h.clock.Set(day.EndsAt.Add(-time.Second))
	gate := newWriteGate(t, "entry")
	h.service = app.New(gatedRepository{Repository: h.store, gate: gate}, h.messenger, h.clock, firstCandidate{})
	commandDone := make(chan error, 1)
	go func() { _, err := h.service.Execute(ctx, h.command("check")); commandDone <- err }()
	gate.wait(t)
	h.clock.Set(day.EndsAt)
	drawDone := make(chan error, 1)
	go func() { _, err := h.service.RunDraw(ctx, h.run.CampaignID, day.DrawID, "test-admin"); drawDone <- err }()
	h.waitForLock(t, "FROM draws WHERE id=$1 FOR UPDATE")
	gate.resume()
	must(t, <-commandDone)
	must(t, <-drawDone)
	draw, err := h.store.Draw(ctx, day.DrawID)
	must(t, err)
	expected := []domain.Candidate{{UserID: h.run.UserIDs[0], Weight: 1}}
	if !reflect.DeepEqual(draw.Candidates, expected) {
		t.Fatalf("accepted registration missing from snapshot: %+v", draw.Candidates)
	}
	stats, err := h.store.Stats(ctx, h.run.CampaignID)
	must(t, err)
	if stats.Entries[day.DrawID] != 1 {
		t.Fatal("the accepted registration was not persisted")
	}
	active, err := h.store.Activities(ctx, h.run.CampaignID, h.run.UserIDs[0])
	must(t, err)
	if len(active) != 1 {
		t.Fatal("accepted registration lost its daily chance")
	}
}

func TestApplicationRegistrationWaitingForDailyCloseCannotEnterSnapshotLate(t *testing.T) {
	h := setup(t, "standard")
	ctx := context.Background()
	day, err := h.store.CurrentDay(ctx, h.run.CampaignID, h.clock.Now())
	must(t, err)
	h.clock.Set(day.EndsAt.Add(-time.Second))
	release := h.holdLock(t, func(tx app.Tx) error { _, err := tx.Draw(ctx, day.DrawID, true); return err })
	type commandResult struct {
		reply domain.Message
		err   error
	}
	commandDone := make(chan commandResult, 1)
	go func() {
		reply, err := h.service.Execute(ctx, h.command("check"))
		commandDone <- commandResult{reply: reply, err: err}
	}()
	h.waitForLock(t, "FROM draws WHERE id=$1 FOR SHARE")
	h.clock.Set(day.EndsAt)
	drawDone := make(chan error, 1)
	go func() { _, err := h.service.RunDraw(ctx, h.run.CampaignID, day.DrawID, "test-admin"); drawDone <- err }()
	h.waitForLock(t, "FROM draws WHERE id=$1 FOR UPDATE")
	release()
	result := <-commandDone
	must(t, result.err)
	must(t, <-drawDone)
	if !strings.Contains(result.reply.Text, "закрыт") {
		t.Fatalf("late registration was not rejected: %s", result.reply.Text)
	}
	draw, err := h.store.Draw(ctx, day.DrawID)
	must(t, err)
	stats, err := h.store.Stats(ctx, h.run.CampaignID)
	must(t, err)
	active, err := h.store.Activities(ctx, h.run.CampaignID, h.run.UserIDs[0])
	must(t, err)
	if len(draw.Candidates) != 0 || stats.Entries[day.DrawID] != 0 || len(active) != 0 {
		t.Fatalf("late registration changed results: candidates=%+v entries=%d activities=%+v", draw.Candidates, stats.Entries[day.DrawID], active)
	}
}

func TestApplicationBothActivityPathsAreIncludedBeforeFinalFreeze(t *testing.T) {
	for _, action := range []string{"promo", "check"} {
		t.Run(action, func(t *testing.T) {
			h := setup(t, "standard")
			ctx := context.Background()
			campaign, err := h.store.Campaign(ctx, h.run.CampaignID)
			must(t, err)
			h.clock.Set(campaign.EndsAt.Add(-time.Second))
			day, err := h.store.CurrentDay(ctx, campaign.ID, h.clock.Now())
			must(t, err)
			gate := newWriteGate(t, "activity")
			h.service = app.New(gatedRepository{Repository: h.store, gate: gate}, h.messenger, h.clock, firstCandidate{})
			commandDone := make(chan error, 1)
			go func() { _, err := h.service.Execute(ctx, h.command(action)); commandDone <- err }()
			gate.wait(t)
			h.clock.Set(campaign.EndsAt)
			drawDone := make(chan error, 1)
			go func() {
				_, err := h.service.RunDraw(ctx, campaign.ID, campaign.FinalDrawID, "test-admin")
				drawDone <- err
			}()
			h.waitForLock(t, "FROM campaigns WHERE id=$1 FOR UPDATE")
			gate.resume()
			must(t, <-commandDone)
			must(t, <-drawDone)
			draw, err := h.store.Draw(ctx, campaign.FinalDrawID)
			must(t, err)
			active, err := h.store.Activities(ctx, campaign.ID, h.run.UserIDs[0])
			must(t, err)
			expected := []domain.Candidate{{UserID: h.run.UserIDs[0], Weight: 1}}
			if !reflect.DeepEqual(draw.Candidates, expected) || len(active) != 1 || active[0].DayID != day.ID {
				t.Fatalf("accepted %s activity lost during final close: candidates=%+v active=%+v", action, draw.Candidates, active)
			}
			reply := h.execute(t, h.command(action))
			if !strings.Contains(reply.Text, "нет активного") {
				t.Fatalf("closed campaign accepted %s: %s", action, reply.Text)
			}
			after, err := h.store.Draw(ctx, campaign.FinalDrawID)
			must(t, err)
			active, err = h.store.Activities(ctx, campaign.ID, h.run.UserIDs[0])
			must(t, err)
			if !reflect.DeepEqual(after.Candidates, expected) || len(active) != 1 {
				t.Fatal("activity changed after final snapshot was frozen")
			}
		})
	}
}
