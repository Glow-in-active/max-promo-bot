//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"promo/internal/app"
	"promo/internal/domain"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Fatal("integration tests require TEST_DATABASE_URL pointing to a disposable PostgreSQL database")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + newID()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	if err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.Close()
		_, e := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		if e != nil {
			t.Error(e)
		}
		admin.Close(ctx)
	})
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

func fixture(t *testing.T, s *Store, scenario string) domain.DemoRun {
	t.Helper()
	r, err := s.CreateDemo(context.Background(), scenario, time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func issue(ctx context.Context, s *Store, r domain.DemoRun, user int64) (string, error) {
	var value string
	err := s.Within(ctx, func(tx app.Tx) error {
		c, err := tx.Campaign(ctx, r.CampaignID, false)
		if err != nil {
			return err
		}
		if err = c.CanAct(r.Now); err != nil {
			return err
		}
		d, err := tx.DayAt(ctx, r.CampaignID, r.Now)
		if err != nil {
			return err
		}
		if _, err = tx.UserForUpdate(ctx, user, r.Now); err != nil {
			return err
		}
		p, err := tx.Assignment(ctx, d.ID, user)
		if err != nil {
			return err
		}
		if p != nil {
			value = p.Value
			return nil
		}
		if err = tx.LockPool(ctx, d.ID); err != nil {
			return err
		}
		p, err = tx.NextPromo(ctx, d.ID)
		if err != nil {
			return err
		}
		if p == nil {
			return domain.Fail(domain.CodesExhausted, "empty")
		}
		active, err := tx.Activities(ctx, c.ID, user)
		if err != nil {
			return err
		}
		grant, err := domain.GrantChance(active, d)
		if err != nil {
			return err
		}
		if err = tx.AssignPromo(ctx, p.ID, user, r.Now); err != nil {
			return err
		}
		if grant {
			if err = tx.AddActivity(ctx, c.ID, user, d, "promo", r.Now); err != nil {
				return err
			}
		}
		value = p.Value
		return nil
	})
	return value, err
}

func TestMigrateIsIdempotentAndDemoIsolated(t *testing.T) {
	s := testStore(t)
	requireNoError(t, s.Migrate(context.Background()))
	a := fixture(t, s, "standard")
	b := fixture(t, s, "standard")
	if a.CampaignID == b.CampaignID || a.UserIDs[0] == b.UserIDs[0] {
		t.Fatal("demo data is shared")
	}
	da, err := s.CurrentDay(context.Background(), a.CampaignID, a.Now)
	requireNoError(t, err)
	db, err := s.CurrentDay(context.Background(), b.CampaignID, b.Now)
	requireNoError(t, err)
	if da.ChannelID == db.ChannelID {
		t.Fatal("subscription overrides would leak between runs")
	}
	stats, err := s.Stats(context.Background(), a.CampaignID)
	requireNoError(t, err)
	if stats.Users != 8 || remainingForDay(t, stats, a.DayID) != 20 {
		t.Fatalf("unexpected statistics: %+v", stats)
	}
}

func TestConcurrentSameUserGetsOneCodeAndChance(t *testing.T) {
	s := testStore(t)
	r := fixture(t, s, "standard")
	const count = 40
	start := make(chan struct{})
	values := make(chan string, count)
	failures := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := issue(context.Background(), s, r, r.UserIDs[0])
			if err != nil {
				failures <- err
			} else {
				values <- v
			}
		}()
	}
	close(start)
	wg.Wait()
	close(values)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	first := ""
	for v := range values {
		if first == "" {
			first = v
		}
		if first != v {
			t.Fatal("one user received different codes")
		}
	}
	active, err := s.Activities(context.Background(), r.CampaignID, r.UserIDs[0])
	requireNoError(t, err)
	if len(active) != 1 {
		t.Fatalf("got %d chances", len(active))
	}
	stats, err := s.Stats(context.Background(), r.CampaignID)
	requireNoError(t, err)
	if stats.PromoIssued != 1 {
		t.Fatalf("issued %d codes", stats.PromoIssued)
	}
}

func TestConcurrentUsersCannotShareLastCode(t *testing.T) {
	s := testStore(t)
	r := fixture(t, s, "standard")
	_, err := s.pool.Exec(context.Background(), "DELETE FROM promos WHERE day_id=$1 AND id<>(SELECT min(id) FROM promos WHERE day_id=$1)", r.DayID)
	requireNoError(t, err)
	var successes, exhausted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, user := range r.UserIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := issue(context.Background(), s, r, user)
			if err == nil {
				successes.Add(1)
				return
			}
			if fault, ok := domain.FaultOf(err); ok && fault.Code == domain.CodesExhausted {
				exhausted.Add(1)
			} else {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if successes.Load() != 1 || exhausted.Load() != 7 {
		t.Fatalf("success=%d exhausted=%d", successes.Load(), exhausted.Load())
	}
}

func TestBusinessMutationAndInboxCompletionRollbackTogether(t *testing.T) {
	s := testStore(t)
	r := fixture(t, s, "standard")
	ctx := context.Background()
	cmd := domain.Command{ID: newID(), CampaignID: r.CampaignID, UserID: r.UserIDs[0], Action: "start"}
	inserted, err := s.PutInbox(ctx, cmd, "unique-event")
	requireNoError(t, err)
	if !inserted {
		t.Fatal("event not inserted")
	}
	sentinel := errors.New("abort business transaction")
	err = s.Within(ctx, func(tx app.Tx) error {
		if e := tx.CompleteInbox(ctx, cmd.ID); e != nil {
			return e
		}
		if e := tx.Enqueue(ctx, domain.Message{ID: "rolled-back", CampaignID: r.CampaignID, UserID: cmd.UserID, Text: "hello"}, r.CampaignID, r.Now); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	err = s.Within(ctx, func(tx app.Tx) error {
		done, e := tx.InboxDone(ctx, cmd.ID)
		if done {
			t.Fatal("inbox completed despite rollback")
		}
		return e
	})
	requireNoError(t, err)
	messages, err := s.Messages(ctx, r.CampaignID, "")
	requireNoError(t, err)
	if len(messages) != 0 {
		t.Fatal("outbox committed despite rollback")
	}
}

func TestTransactionRetriesOnlyRollbackSafeFailures(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	attempts := 0
	err := s.Within(ctx, func(tx app.Tx) error {
		attempts++
		_, e := tx.UserForUpdate(ctx, 50, time.Now())
		if e != nil {
			return e
		}
		if attempts < 3 {
			return &pgconn.PgError{Code: "40001", Message: "forced serialization failure"}
		}
		return nil
	})
	requireNoError(t, err)
	if attempts != 3 {
		t.Fatalf("attempts=%d", attempts)
	}
	attempts = 0
	sentinel := errors.New("do not retry")
	err = s.Within(ctx, func(app.Tx) error { attempts++; return sentinel })
	if !errors.Is(err, sentinel) || attempts != 1 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}

func TestInboxLeaseDedupAndRecovery(t *testing.T) {
	s := testStore(t)
	r := fixture(t, s, "standard")
	ctx := context.Background()
	command := domain.Command{ID: newID(), CampaignID: r.CampaignID, UserID: r.UserIDs[0], Action: "promo"}
	yes, err := s.PutInbox(ctx, command, "provider-event")
	requireNoError(t, err)
	if !yes {
		t.Fatal("first insert rejected")
	}
	yes, err = s.PutInbox(ctx, command, "provider-event")
	requireNoError(t, err)
	if yes {
		t.Fatal("duplicate inserted")
	}
	// PutInbox uses database time; move eligibility into the deterministic clock.
	_, err = s.pool.Exec(ctx, "UPDATE inbox SET next_attempt_at=$1", r.Now)
	requireNoError(t, err)
	first, err := s.TakeInbox(ctx, r.Now)
	requireNoError(t, err)
	if first == nil || first.Attempts != 1 {
		t.Fatalf("first=%+v", first)
	}
	other, err := s.TakeInbox(ctx, r.Now.Add(29*time.Second))
	requireNoError(t, err)
	if other != nil {
		t.Fatal("active lease stolen")
	}
	recovered, err := s.TakeInbox(ctx, r.Now.Add(31*time.Second))
	requireNoError(t, err)
	if recovered == nil || recovered.ID != first.ID || recovered.Attempts != 2 {
		t.Fatalf("recovered=%+v", recovered)
	}
	requireNoError(t, s.Within(ctx, func(tx app.Tx) error { return tx.CompleteInbox(ctx, first.ID) }))
	other, err = s.TakeInbox(ctx, r.Now.Add(time.Hour))
	requireNoError(t, err)
	if other != nil {
		t.Fatal("completed event replayed")
	}
}

func TestOutboxDeliveryDoesNotStartPrizeDeadline(t *testing.T) {
	s := testStore(t)
	r := fixture(t, s, "expired")
	ctx := context.Background()
	before, err := s.Offer(ctx, r.OfferID)
	requireNoError(t, err)
	requireNoError(t, s.Within(ctx, func(tx app.Tx) error {
		return tx.Enqueue(ctx, domain.Message{ID: "notice", CampaignID: r.CampaignID, UserID: before.UserID, Text: "prize", OfferID: before.ID}, r.CampaignID, r.Now)
	}))
	m, err := s.TakeMessage(ctx, r.Now)
	requireNoError(t, err)
	if m == nil || m.Attempts != 1 {
		t.Fatalf("message=%+v", m)
	}
	m, err = s.TakeMessage(ctx, r.Now.Add(31*time.Second))
	requireNoError(t, err)
	if m == nil || m.Attempts != 2 {
		t.Fatalf("recovered message=%+v", m)
	}
	requireNoError(t, s.RetryMessage(ctx, m.ID, "delivery uncertain", r.Now, true))
	failed, err := s.Offer(ctx, r.OfferID)
	requireNoError(t, err)
	if failed.DeliveryState != "failed" || failed.DeliveryError != "delivery uncertain" {
		t.Fatalf("failed offer delivery=%+v", failed)
	}
	requireNoError(t, s.Within(ctx, func(tx app.Tx) error {
		return tx.RetryOfferMessage(ctx, r.OfferID, r.Now)
	}))
	m, err = s.TakeMessage(ctx, r.Now)
	requireNoError(t, err)
	if m == nil || m.Attempts != 1 {
		t.Fatalf("administrator retry did not reset attempts: %+v", m)
	}
	requireNoError(t, s.FinishMessage(ctx, "notice"))
	after, err := s.Offer(ctx, r.OfferID)
	requireNoError(t, err)
	if !after.Deadline.Equal(*before.Deadline) || !after.SentAt.Equal(*before.SentAt) {
		t.Fatal("delivery changed prize state")
	}
	messages, err := s.Messages(ctx, r.CampaignID, "")
	requireNoError(t, err)
	if len(messages) != 1 || messages[0].State != "delivered" {
		t.Fatalf("messages=%+v", messages)
	}
	messages, err = s.Messages(ctx, r.CampaignID, "notice")
	requireNoError(t, err)
	if len(messages) != 0 {
		t.Fatal("cursor repeated a message")
	}
}

func TestFinalWeightsAndImmutableSnapshot(t *testing.T) {
	s := testStore(t)
	r := fixture(t, s, "final")
	ctx := context.Background()
	requireNoError(t, s.Within(ctx, func(tx app.Tx) error {
		d, err := tx.Draw(ctx, r.FinalDrawID, true)
		if err != nil {
			return err
		}
		d.Candidates, err = tx.Candidates(ctx, d)
		if err != nil {
			return err
		}
		if len(d.Candidates) != 8 {
			t.Fatal("missing candidates")
		}
		for i, c := range d.Candidates {
			if c.Weight != i+1 {
				t.Fatalf("candidate=%+v", c)
			}
		}
		d.State = domain.DrawDrawn
		d.DrawnAt = &r.Now
		d.Selection = domain.Selection{Winners: []int64{r.UserIDs[0]}, Reserve: []int64{r.UserIDs[1]}}
		return tx.SaveDraw(ctx, d)
	}))
	requireNoError(t, s.Within(ctx, func(tx app.Tx) error {
		d, err := tx.Draw(ctx, r.FinalDrawID, true)
		if err != nil {
			return err
		}
		d.State = domain.DrawApproved
		d.ApprovedAt = &r.Now
		d.Candidates = nil
		d.Selection = domain.Selection{}
		return tx.SaveDraw(ctx, d)
	}))
	d, err := s.Draw(ctx, r.FinalDrawID)
	requireNoError(t, err)
	if len(d.Candidates) != 8 || len(d.Selection.Winners) != 1 {
		t.Fatal("immutable snapshot was overwritten")
	}
}

func TestDemoScenariosAreComplete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, scenario := range []string{"standard", "exhausted", "drawable", "final", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			r := fixture(t, s, scenario)
			draws, err := s.Draws(ctx, r.CampaignID)
			requireNoError(t, err)
			if len(draws) != 11 {
				t.Fatalf("draw count=%d", len(draws))
			}
			if scenario == "expired" {
				offers, err := s.DueOffers(ctx, r.Now)
				requireNoError(t, err)
				if len(offers) != 1 || offers[0].ID != r.OfferID {
					t.Fatal("expired offer missing")
				}
			}
			if scenario == "exhausted" {
				stats, err := s.Stats(ctx, r.CampaignID)
				requireNoError(t, err)
				if remainingForDay(t, stats, r.DayID) != 0 {
					t.Fatal("pool not empty")
				}
			}
		})
	}
}

func importFixture() domain.Setup {
	return importFixtureInZone(time.UTC)
}

func importFixtureInZone(zone *time.Location) domain.Setup {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, zone)
	campaignID := newID()
	finalID := newID()
	setup := domain.Setup{Campaign: domain.Campaign{ID: campaignID, Name: "Imported campaign", Status: "active", StartsAt: start, EndsAt: start.AddDate(0, 0, 10), Timezone: zone.String(), FinalDrawID: finalID, MainPrize: "Concert tickets", Rules: "Ten daily chances"}}
	for i := range 10 {
		d := domain.Day{ID: newID(), CampaignID: campaignID, Number: i + 1, StartsAt: start.AddDate(0, 0, i), EndsAt: start.AddDate(0, 0, i+1), Partner: fmt.Sprintf("Partner %d", i+1), PartnerURL: "https://max.ru/partner", ChannelID: -100 - int64(i), Instruction: "Join the partner channel.", DrawID: newID()}
		setup.Days = append(setup.Days, d)
		setup.Draws = append(setup.Draws, domain.Draw{ID: d.DrawID, CampaignID: campaignID, DayID: d.ID, Kind: "daily", State: domain.DrawOpen, ClosesAt: d.EndsAt, WinnerCount: 1, ReserveCount: 2, Prize: "Partner gift", ClaimTTL: 24 * time.Hour})
		setup.Promos = append(setup.Promos, domain.Promo{ID: newID(), DayID: d.ID, Value: newID()})
	}
	setup.Draws = append(setup.Draws, domain.Draw{ID: finalID, CampaignID: campaignID, Kind: "final", State: domain.DrawOpen, ClosesAt: setup.Campaign.EndsAt, WinnerCount: 5, ReserveCount: 3, Prize: "Concert tickets", ClaimTTL: 48 * time.Hour})
	return setup
}

func TestSetupImportNeverOverwritesAndFailureRollsBack(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	setup := importFixture()
	requireNoError(t, s.Setup(ctx, setup))
	d, err := s.CurrentDay(ctx, setup.Campaign.ID, setup.Campaign.StartsAt.Add(time.Hour))
	requireNoError(t, err)
	if d.ID != setup.Days[0].ID {
		t.Fatal("imported day missing")
	}
	err = s.Setup(ctx, setup)
	fault, ok := domain.FaultOf(err)
	if !ok || fault.Code != domain.InvalidTransition {
		t.Fatalf("duplicate import err=%v", err)
	}
	// This failure passes domain validation but conflicts with another campaign's code.
	conflicting := importFixture()
	conflicting.Promos[9].Value = setup.Promos[0].Value
	if err = s.Setup(ctx, conflicting); err == nil {
		t.Fatal("duplicate code import succeeded")
	}
	_, err = s.Campaign(ctx, conflicting.Campaign.ID)
	fault, ok = domain.FaultOf(err)
	if !ok || fault.Code != domain.NotFound {
		t.Fatalf("failed import left a campaign: %v", err)
	}
	var count int
	requireNoError(t, s.pool.QueryRow(ctx, "SELECT count(*) FROM days WHERE campaign_id=$1", conflicting.Campaign.ID).Scan(&count))
	if count != 0 {
		t.Fatal("failed import left partial days")
	}
	stats, err := s.Stats(ctx, setup.Campaign.ID)
	requireNoError(t, err)
	if len(stats.PromoRemaining) != 10 {
		t.Fatal("original campaign was modified")
	}
}

func TestProductionExampleCanBeImported(t *testing.T) {
	s := testStore(t)
	data, err := os.ReadFile("../../../config/campaign.example.json")
	requireNoError(t, err)
	var setup domain.Setup
	requireNoError(t, json.Unmarshal(data, &setup))
	setup, err = setup.Prepare()
	requireNoError(t, err)
	requireNoError(t, s.Setup(context.Background(), setup))
	stats, err := s.Stats(context.Background(), setup.Campaign.ID)
	requireNoError(t, err)
	if len(stats.PromoRemaining) != 10 || stats.Users != 0 {
		t.Fatalf("imported example statistics: %+v", stats)
	}
}

func TestInvalidCampaignImportLeavesNoData(t *testing.T) {
	cases := map[string]func(*domain.Setup){
		"empty timezone": func(setup *domain.Setup) { setup.Campaign.Timezone = "" },
		"host timezone":  func(setup *domain.Setup) { setup.Campaign.Timezone = "Local" },
		"no final reserve": func(setup *domain.Setup) {
			for i := range setup.Draws {
				if setup.Draws[i].Kind == "final" {
					setup.Draws[i].ReserveCount = 0
				}
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			setup := importFixture()
			change(&setup)
			err := s.Setup(ctx, setup)
			fault, ok := domain.FaultOf(err)
			if !ok || fault.Code != domain.Invalid {
				t.Fatalf("invalid import returned %v", err)
			}
			var rows int
			requireNoError(t, s.pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM campaigns) + (SELECT count(*) FROM days) +
				(SELECT count(*) FROM draws) + (SELECT count(*) FROM promos)`).Scan(&rows))
			if rows != 0 {
				t.Fatalf("invalid import left %d rows", rows)
			}
		})
	}
}

func TestImportedTimezoneControlsStatisticsCalendar(t *testing.T) {
	for zone, day := range map[string]string{"UTC": "2026-11-01", "Europe/Moscow": "2026-11-02"} {
		t.Run(zone, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			location, err := time.LoadLocation(zone)
			requireNoError(t, err)
			setup := importFixtureInZone(location)
			requireNoError(t, s.Setup(ctx, setup))
			started := time.Date(2026, 11, 1, 21, 30, 0, 0, time.UTC)
			requireNoError(t, s.Within(ctx, func(tx app.Tx) error {
				if _, err := tx.UserForUpdate(ctx, 42, started); err != nil {
					return err
				}
				return tx.TouchCampaignUser(ctx, setup.Campaign.ID, 42, started)
			}))
			stats, err := s.Stats(ctx, setup.Campaign.ID)
			requireNoError(t, err)
			if stats.Users != 1 || !reflect.DeepEqual(stats.NewUsersByDay, map[string]int{day: 1}) {
				t.Fatalf("timezone %s: statistics=%+v", zone, stats)
			}
		})
	}
}

func remainingForDay(t *testing.T, stats domain.Stats, dayID string) int {
	t.Helper()
	for _, partner := range stats.PromoRemaining {
		for _, day := range partner.Days {
			if day.DayID == dayID {
				return day.Remaining
			}
		}
	}
	t.Fatalf("no promo stock for day %s", dayID)
	return 0
}

func TestStatsAggregateRemainingCodesByPartnerIdentity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	setup := importFixture()
	setup.Days[0].Partner = "Партнёр А"
	setup.Days[1].ChannelID = setup.Days[0].ChannelID
	setup.Days[1].Partner = "Партнёр А, второй день"
	setup.Days[2].Partner = setup.Days[0].Partner
	for _, day := range []int{0, 0, 1} {
		setup.Promos = append(setup.Promos, domain.Promo{ID: newID(), DayID: setup.Days[day].ID, Value: newID()})
	}
	requireNoError(t, s.Setup(ctx, setup))
	run := domain.DemoRun{CampaignID: setup.Campaign.ID, Now: setup.Campaign.StartsAt.Add(time.Hour)}
	_, err := issue(ctx, s, run, 50)
	requireNoError(t, err)
	stats, err := s.Stats(ctx, setup.Campaign.ID)
	requireNoError(t, err)
	if len(stats.PromoRemaining) != 9 {
		t.Fatalf("partners=%d expected 9", len(stats.PromoRemaining))
	}
	repeated := stats.PromoRemaining[0]
	if repeated.ChannelID != setup.Days[0].ChannelID || repeated.Partner != "Партнёр А" || repeated.Remaining != 4 {
		t.Fatalf("repeated partner=%+v", repeated)
	}
	expectedDays := []domain.DayPromoStock{{DayID: setup.Days[0].ID, DayNumber: 1, Remaining: 2}, {DayID: setup.Days[1].ID, DayNumber: 2, Remaining: 2}}
	if !reflect.DeepEqual(repeated.Days, expectedDays) {
		t.Fatalf("day details=%+v expected %+v", repeated.Days, expectedDays)
	}
	if stats.PromoRemaining[1].ChannelID != setup.Days[2].ChannelID || stats.PromoRemaining[1].Remaining != 1 {
		t.Fatal("different partners with matching names were merged")
	}
}

func TestStatsKeepNamedZeroStocksForEmptyAndExhaustedPools(t *testing.T) {
	for _, assigned := range []bool{false, true} {
		name := "never_loaded"
		if assigned {
			name = "fully_assigned"
		}
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			setup := importFixture()
			if !assigned {
				setup.Promos = nil
			}
			requireNoError(t, s.Setup(ctx, setup))
			wantIssued := 0
			if assigned {
				_, err := s.pool.Exec(ctx, "INSERT INTO users(id,started_at) VALUES(50,$1)", setup.Campaign.StartsAt)
				requireNoError(t, err)
				_, err = s.pool.Exec(ctx, "UPDATE promos SET user_id=50,assigned_at=$1", setup.Campaign.StartsAt)
				requireNoError(t, err)
				wantIssued = 10
			}
			stats, err := s.Stats(ctx, setup.Campaign.ID)
			requireNoError(t, err)
			if len(stats.PromoRemaining) != 10 || stats.PromoIssued != wantIssued {
				t.Fatalf("zero stock groups=%d issued=%d expected issued=%d", len(stats.PromoRemaining), stats.PromoIssued, wantIssued)
			}
			for i, partner := range stats.PromoRemaining {
				day := setup.Days[i]
				if partner.ChannelID != day.ChannelID || partner.Partner != day.Partner || partner.Remaining != 0 {
					t.Fatalf("zero partner missing identity or name: %+v", partner)
				}
				wantDays := []domain.DayPromoStock{{DayID: day.ID, DayNumber: day.Number, Remaining: 0}}
				if !reflect.DeepEqual(partner.Days, wantDays) {
					t.Fatalf("zero day details=%+v expected %+v", partner.Days, wantDays)
				}
			}
		})
	}
}
