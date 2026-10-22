//go:build integration

package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/app"
	"promo/internal/domain"
	"promo/internal/platform/demo"
	"promo/internal/storage/postgres"
	"promo/internal/testdb"
	"promo/internal/transport/httpapi/generated"
)

func postgresHTTPTestStore(t *testing.T) (*postgres.Store, *pgx.Conn) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Fatal("integration tests require TEST_DATABASE_URL for a disposable database")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "http_" + eventID("test", time.Now().String())[:20]
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	repo, err := postgres.New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		repo.Close()
		_, cleanupErr := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		if cleanupErr != nil {
			t.Error(cleanupErr)
		}
		_ = admin.Close(ctx)
	})
	if err = repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = testdb.IsolateDemoUsers(ctx, admin, schema); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "SET search_path TO "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	return repo, admin
}

func TestWebhookInboxIdentityWithPostgres(t *testing.T) {
	ctx := context.Background()
	repo, _ := postgresHTTPTestStore(t)
	run, err := repo.CreateDemo(ctx, "standard", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	h := New(&serviceStub{}, repo, demo.New(), demo.New(), Config{Mode: "production", WebhookSecret: "secret", CampaignID: run.CampaignID})
	for _, test := range []struct {
		mid       string
		duplicate bool
	}{{"first", false}, {"second", false}, {"first", true}} {
		body := `{"update_type":"message_created","timestamp":1,"message":{"sender":{"user_id":42},"recipient":{"chat_type":"dialog"},"body":{"mid":"` + test.mid + `","text":"/help"}}}`
		response := request(h, "POST", "/webhooks/max", body, "")
		if response.Code != 200 {
			t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
		}
		var result struct {
			Duplicate bool `json:"duplicate"`
		}
		if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Duplicate != test.duplicate {
			t.Fatalf("mid=%s duplicate=%v wanted %v", test.mid, result.Duplicate, test.duplicate)
		}
	}
	for range 2 {
		event, takeErr := repo.TakeInbox(ctx, time.Now())
		if takeErr != nil || event == nil {
			t.Fatalf("expected two independent events: %+v %v", event, takeErr)
		}
	}
	third, err := repo.TakeInbox(ctx, time.Now())
	if err != nil || third != nil {
		t.Fatalf("duplicate created another event: %+v %v", third, err)
	}
}

type statsClock struct{ now time.Time }

func (c *statsClock) Now() time.Time { return c.now }

func TestStatsAPIReportsRepeatedPartnerNameAndTotal(t *testing.T) {
	ctx := context.Background()
	repo, fixture := postgresHTTPTestStore(t)
	clock := &statsClock{now: time.Now().UTC()}
	run, err := repo.CreateDemo(ctx, "standard", clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	firstDay, err := repo.CurrentDay(ctx, run.CampaignID, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	const partnerName = "Одинаковое имя партнёров"
	if _, err = fixture.Exec(ctx, "UPDATE days SET channel_id=$1,partner=$2 WHERE campaign_id=$3 AND number IN (1,2)", firstDay.ChannelID, partnerName, run.CampaignID); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.Exec(ctx, "UPDATE days SET partner=$1 WHERE campaign_id=$2 AND number=3", partnerName, run.CampaignID); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.Exec(ctx, "DELETE FROM promos WHERE day_id IN (SELECT id FROM days WHERE campaign_id=$1) AND id NOT IN (SELECT min(id) FROM promos GROUP BY day_id)", run.CampaignID); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.Exec(ctx, "DELETE FROM promos WHERE day_id IN (SELECT id FROM days WHERE campaign_id=$1 AND number=3)", run.CampaignID); err != nil {
		t.Fatal(err)
	}
	var secondDayID, emptyDayID string
	var independentChannel int64
	if err = fixture.QueryRow(ctx, "SELECT id FROM days WHERE campaign_id=$1 AND number=2", run.CampaignID).Scan(&secondDayID); err != nil {
		t.Fatal(err)
	}
	if err = fixture.QueryRow(ctx, "SELECT id,channel_id FROM days WHERE campaign_id=$1 AND number=3", run.CampaignID).Scan(&emptyDayID, &independentChannel); err != nil {
		t.Fatal(err)
	}
	messenger := demo.New()
	service := app.New(repo, messenger, clock, domain.CryptoRandom{})
	h := New(service, repo, messenger, messenger, Config{Mode: "production", AdminToken: "admin", CampaignID: run.CampaignID})
	_, router := contractRouter(t)
	path := "/api/admin/campaigns/" + run.CampaignID + "/stats"
	assertSnapshot := func(issued, requests, firstRemaining, secondRemaining int) {
		t.Helper()
		response := request(h, "GET", path, "", "admin")
		if response.Code != 200 {
			t.Fatalf("stats HTTP %d: %s", response.Code, response.Body.String())
		}
		assertResponseContract(t, router, "GET", path, response)
		var stats generated.Stats
		if err := json.Unmarshal(response.Body.Bytes(), &stats); err != nil {
			t.Fatal(err)
		}
		if stats.PromoIssued != issued || stats.PromoRequests != requests || len(stats.PromoRemaining) != 9 {
			t.Fatalf("wrong inventory counters: %+v", stats)
		}
		channels := map[int64]bool{}
		days := map[string]bool{}
		numbers := map[int]bool{}
		total := 0
		var repeated, independent *generated.PartnerPromoStock
		for i := range stats.PromoRemaining {
			partner := &stats.PromoRemaining[i]
			if channels[partner.ChannelID] || partner.Partner == "" {
				t.Fatalf("duplicate or unnamed partner: %+v", partner)
			}
			channels[partner.ChannelID] = true
			detailTotal := 0
			for _, day := range partner.Days {
				if days[day.DayID] || numbers[day.DayNumber] || day.Remaining < 0 {
					t.Fatalf("invalid or duplicated day detail: %+v", day)
				}
				days[day.DayID] = true
				numbers[day.DayNumber] = true
				detailTotal += day.Remaining
			}
			if detailTotal != partner.Remaining {
				t.Fatalf("partner total differs from detail: %+v", partner)
			}
			total += partner.Remaining
			if partner.ChannelID == firstDay.ChannelID {
				repeated = partner
			}
			if partner.ChannelID == independentChannel {
				independent = partner
			}
		}
		if total+stats.PromoIssued != 9 || len(days) != 10 || len(numbers) != 10 {
			t.Fatalf("inventory is not conserved: total=%d issued=%d days=%d numbers=%d", total, stats.PromoIssued, len(days), len(numbers))
		}
		wantRepeated := generated.PartnerPromoStock{ChannelID: firstDay.ChannelID, Partner: partnerName, Remaining: firstRemaining + secondRemaining, Days: []generated.DayPromoStock{{DayID: firstDay.ID, DayNumber: 1, Remaining: firstRemaining}, {DayID: secondDayID, DayNumber: 2, Remaining: secondRemaining}}}
		if repeated == nil || !reflect.DeepEqual(*repeated, wantRepeated) {
			t.Fatalf("repeated partner=%+v expected %+v", repeated, wantRepeated)
		}
		wantIndependent := generated.PartnerPromoStock{ChannelID: independentChannel, Partner: partnerName, Remaining: 0, Days: []generated.DayPromoStock{{DayID: emptyDayID, DayNumber: 3, Remaining: 0}}}
		if independent == nil || !reflect.DeepEqual(*independent, wantIndependent) {
			t.Fatalf("same name on another channel merged or zero pool omitted: %+v", independent)
		}
	}
	issue := func(user int64) domain.Message {
		t.Helper()
		reply, err := service.Execute(ctx, domain.Command{ID: rand.Text(), CampaignID: run.CampaignID, UserID: user, Action: "promo", OccurredAt: clock.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	assertSnapshot(0, 0, 1, 1)
	first := issue(run.UserIDs[0])
	if !strings.Contains(first.Text, "Промокод:") {
		t.Fatalf("first code was not issued: %s", first.Text)
	}
	assertSnapshot(1, 1, 0, 1)
	replay := issue(run.UserIDs[0])
	if replay.Text != first.Text {
		t.Fatalf("replay changed the assigned code: first=%s replay=%s", first.Text, replay.Text)
	}
	assertSnapshot(1, 2, 0, 1)
	exhausted := issue(run.UserIDs[1])
	if !strings.Contains(exhausted.Text, "закончились") {
		t.Fatalf("exhausted pool was not reported: %s", exhausted.Text)
	}
	assertSnapshot(1, 3, 0, 1)
	clock.now = firstDay.EndsAt.Add(time.Hour)
	second := issue(run.UserIDs[0])
	if !strings.Contains(second.Text, "Промокод:") || second.Text == first.Text {
		t.Fatalf("second day code was not issued: %s", second.Text)
	}
	assertSnapshot(2, 4, 0, 0)
}
