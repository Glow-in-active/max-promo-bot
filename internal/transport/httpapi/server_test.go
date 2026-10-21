package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
	"promo/internal/platform/demo"
)

type repositoryStub struct {
	app.Repository
	pingErr, error error
	command        domain.Command
	duplicate      bool
	run            domain.DemoRun
	messages       []domain.Message
}

func (s *repositoryStub) Ping(context.Context) error { return s.pingErr }
func (s *repositoryStub) PutInbox(_ context.Context, c domain.Command, key string) (bool, error) {
	if key != c.ID {
		panic("inbox deduplication key must equal command ID")
	}
	s.command = c
	return !s.duplicate, s.error
}
func (s *repositoryStub) Campaign(context.Context, string) (domain.Campaign, error) {
	return domain.Campaign{ID: "c"}, s.error
}
func (s *repositoryStub) CreateDemo(context.Context, string, time.Time) (domain.DemoRun, error) {
	return s.run, s.error
}
func (s *repositoryStub) CurrentDay(context.Context, string, time.Time) (domain.Day, error) {
	return domain.Day{ChannelID: 17}, s.error
}
func (s *repositoryStub) Messages(context.Context, string, string) ([]domain.Message, error) {
	return s.messages, s.error
}
func (s *repositoryStub) Draws(context.Context, string) ([]domain.Draw, error) {
	return []domain.Draw{{ID: "d", Kind: "daily", State: domain.DrawApproved, Prize: "=formula", Candidates: []domain.Candidate{{UserID: 42, Weight: 1}}, Selection: domain.Selection{Winners: []int64{42}, Reserve: []int64{43}}}}, s.error
}
func (s *repositoryStub) Stats(context.Context, string) (domain.Stats, error) {
	return domain.Stats{Users: 1}, s.error
}
func (s *repositoryStub) Export(ctx context.Context, campaignID string) (domain.ExportSnapshot, error) {
	draws, err := s.Draws(ctx, campaignID)
	return domain.ExportSnapshot{Campaign: domain.Campaign{ID: campaignID}, Draws: draws, Offers: []domain.Offer{{ID: "offer", DrawID: "d"}}}, err
}
func (s *repositoryStub) Offer(context.Context, string) (domain.Offer, error) {
	return domain.Offer{ID: "offer"}, s.error
}

type serviceStub struct{ err error }

func (s *serviceStub) RunDraw(context.Context, string, string, string) (domain.Draw, error) {
	return domain.Draw{ID: "draw", Kind: "daily", State: domain.DrawDrawn}, s.err
}
func (s *serviceStub) ApproveDraw(context.Context, string, string, string) (domain.Draw, error) {
	return domain.Draw{ID: "draw", Kind: "daily", State: domain.DrawApproved}, s.err
}
func (s *serviceStub) ExpireOffer(context.Context, string) error             { return s.err }
func (s *serviceStub) ConfirmDelivery(context.Context, string, string) error { return s.err }
func (s *serviceStub) RetryDelivery(context.Context, string, string) error   { return s.err }
func (s *serviceStub) ListOffers(context.Context, string, string) ([]domain.Offer, error) {
	return []domain.Offer{{ID: "offer"}}, s.err
}
func (s *serviceStub) Tick(context.Context) error { return s.err }
func (*serviceStub) Now() time.Time               { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }

func handler(repo *repositoryStub, service *serviceStub, mode string) http.Handler {
	return New(service, repo, demo.New(), demo.New(), Config{Mode: mode, AdminToken: "admin", DemoToken: "demo", WebhookSecret: "secret", CampaignID: "campaign"})
}
func request(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-Max-Bot-Api-Secret", "secret")
	r.Header.Set("X-Demo-Campaign", "campaign")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestHealthAndReadiness(t *testing.T) {
	repo := &repositoryStub{}
	h := handler(repo, &serviceStub{}, "production")
	for _, path := range []string{"/healthz", "/readyz"} {
		if got := request(h, "GET", path, "", "").Code; got != 200 {
			t.Fatalf("%s: %d", path, got)
		}
	}
	repo.pingErr = errors.New("database credential")
	w := request(h, "GET", "/readyz", "", "")
	if w.Code != 503 || strings.Contains(w.Body.String(), "credential") {
		t.Fatalf("unexpected readiness: %d %s", w.Code, w.Body.String())
	}
}
func TestAdminAuthentication(t *testing.T) {
	paths := []struct{ method, path string }{{"GET", "/api/admin/campaigns/c/draws"}, {"POST", "/api/admin/campaigns/c/draws/d/run"}, {"POST", "/api/admin/campaigns/c/draws/d/approve"}, {"GET", "/api/admin/campaigns/c/stats"}, {"GET", "/api/admin/campaigns/c/export.csv"}, {"POST", "/api/admin/tick"}, {"GET", "/api/admin/offers/o"}, {"POST", "/api/admin/offers/o/expire"}, {"POST", "/api/admin/offers/o/delivered"}, {"POST", "/api/admin/offers/o/retry"}, {"GET", "/api/admin/campaigns/c/draws/d/offers"}}
	h := handler(&repositoryStub{}, &serviceStub{}, "production")
	for _, path := range paths {
		if got := request(h, path.method, path.path, "", "").Code; got != 401 {
			t.Fatalf("unauthenticated %s: %d", path.path, got)
		}
		if got := request(h, path.method, path.path, "", "admin").Code; got != 200 {
			t.Fatalf("authenticated %s: %d", path.path, got)
		}
	}
}
func TestDemoRoutesAbsentInProduction(t *testing.T) {
	h := handler(&repositoryStub{}, &serviceStub{}, "production")
	for _, path := range []struct{ method, path string }{{"POST", "/_demo/runs"}, {"GET", "/_demo/runs/c/messages"}, {"POST", "/_demo/runs/c/subscription"}} {
		if got := request(h, path.method, path.path, `{"scenario":"standard"}`, "demo").Code; got != 404 {
			t.Fatalf("demo route exposed: %s = %d", path.path, got)
		}
	}
}
func TestCreateDemoValidation(t *testing.T) {
	h := handler(&repositoryStub{run: domain.DemoRun{CampaignID: "fresh"}}, &serviceStub{}, "demo")
	for _, test := range []struct {
		body, token string
		status      int
	}{{`{"scenario":"standard"}`, "demo", 201}, {`{"scenario":"unknown"}`, "demo", 400}, {`{"scenario":"standard","extra":true}`, "demo", 400}, {`{"scenario":"standard"} {}`, "demo", 400}, {`{"scenario":"standard"}`, "", 401}, {`{`, "demo", 400}} {
		w := request(h, "POST", "/_demo/runs", test.body, test.token)
		if w.Code != test.status {
			t.Fatalf("%s: %d %s", test.body, w.Code, w.Body.String())
		}
	}
}
func TestWebhookIsAuthenticatedAndDurable(t *testing.T) {
	repo := &repositoryStub{}
	h := handler(repo, &serviceStub{}, "production")
	body := `{"update_type":"bot_started","timestamp":10,"chat_id":1,"user":{"user_id":42},"new_platform_field":true}`
	w := request(h, "POST", "/webhooks/max", body, "")
	if w.Code != 200 || repo.command.Action != "start" || repo.command.CampaignID != "campaign" {
		t.Fatalf("invalid webhook: %d %s %+v", w.Code, w.Body.String(), repo.command)
	}
	repo.duplicate = true
	w = request(h, "POST", "/webhooks/max", body, "")
	if !strings.Contains(w.Body.String(), `"duplicate":true`) {
		t.Fatal("duplicate not acknowledged")
	}
	r := httptest.NewRequest("POST", "/webhooks/max", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("unauthenticated webhook accepted")
	}
	repo.error = errors.New("private SQL connection password")
	w = request(h, "POST", "/webhooks/max", body, "")
	if w.Code != 500 || strings.Contains(w.Body.String(), "password") {
		t.Fatal("storage failure was acknowledged or leaked")
	}
}
func TestWebhookDoesNotAllowCampaignOverrideInProduction(t *testing.T) {
	repo := &repositoryStub{}
	h := handler(repo, &serviceStub{}, "production")
	w := request(h, "POST", "/webhooks/max", `{"update_type":"bot_started","timestamp":10,"chat_id":1,"user":{"user_id":42}}`, "")
	if w.Code != 200 || repo.command.CampaignID != "campaign" {
		t.Fatal("demo header affected production")
	}
}
func TestBodyLimitsAndMediaType(t *testing.T) {
	h := handler(&repositoryStub{}, &serviceStub{}, "demo")
	w := request(h, "POST", "/_demo/runs", `{"scenario":"`+strings.Repeat("a", maxBodyBytes)+`"}`, "demo")
	if w.Code != 413 {
		t.Fatalf("oversize: %d", w.Code)
	}
	r := httptest.NewRequest("POST", "/_demo/runs", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer demo")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatalf("media type: %d", w.Code)
	}
}
func TestSubscriptionFailureIsControlledByDemo(t *testing.T) {
	repo := &repositoryStub{}
	messenger := demo.New()
	h := New(&serviceStub{}, repo, messenger, messenger, Config{Mode: "demo", DemoToken: "demo"})
	for _, status := range []string{"yes", "no", "error"} {
		w := request(h, "POST", "/_demo/runs/c/subscription", `{"user_id":42,"status":"`+status+`"}`, "demo")
		if w.Code != 200 {
			t.Fatalf("status: %d", w.Code)
		}
	}
	if got, err := messenger.Subscription(context.Background(), 17, 42); got != app.SubscriptionUnknown || err == nil {
		t.Fatal("demo override not applied")
	}
	if got := request(h, "POST", "/_demo/runs/c/subscription", `{"user_id":0,"status":"yes"}`, "demo").Code; got != 400 {
		t.Fatalf("invalid user: %d", got)
	}
}
func TestMessagesAndExport(t *testing.T) {
	repo := &repositoryStub{messages: []domain.Message{{ID: "1", Text: "hello"}}}
	h := handler(repo, &serviceStub{}, "demo")
	w := request(h, "GET", "/_demo/runs/c/messages", "", "demo")
	var response struct {
		Messages []domain.Message `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Messages) != 1 {
		t.Fatalf("messages: %s", w.Body.String())
	}
	w = request(h, "GET", "/api/admin/campaigns/c/export.csv", "", "admin")
	if !strings.Contains(w.Body.String(), "selection,d,daily,approved,winner,1,42,1,'=formula") || !strings.Contains(w.Body.String(), "selection,d,daily,approved,reserve,1,43") || !strings.Contains(w.Body.String(), "participant,d,daily") || !strings.Contains(w.Body.String(), "offer,d,daily") {
		t.Fatalf("CSV: %s", w.Body.String())
	}
	if got := request(h, "GET", "/_demo/runs/c/messages?after="+strings.Repeat("a", 201), "", "demo").Code; got != 400 {
		t.Fatalf("cursor: %d", got)
	}
}
func TestDomainErrorsMapWithoutLeakingInternals(t *testing.T) {
	for _, test := range []struct {
		code   domain.Code
		status int
	}{{domain.Invalid, 400}, {domain.NotFound, 404}, {domain.Forbidden, 403}, {domain.SubscriptionUnavailable, 503}, {domain.InvalidTransition, 409}} {
		w := httptest.NewRecorder()
		handleError(w, domain.Fail(test.code, "safe message"))
		if w.Code != test.status || !strings.Contains(w.Body.String(), "safe message") {
			t.Fatalf("%s: %d", test.code, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handleError(w, context.DeadlineExceeded)
	if w.Code != 503 {
		t.Fatal("deadline not transient")
	}
}
func TestAdminErrors(t *testing.T) {
	repo := &repositoryStub{error: domain.Fail(domain.NotFound, "missing")}
	service := &serviceStub{err: domain.Fail(domain.InvalidTransition, "closed")}
	h := handler(repo, service, "demo")
	for _, path := range []string{"/api/admin/campaigns/c/draws/d/run", "/api/admin/campaigns/c/draws/d/approve", "/api/admin/tick", "/api/admin/offers/o/expire", "/api/admin/offers/o/delivered", "/api/admin/offers/o/retry"} {
		if got := request(h, "POST", path, "", "admin").Code; got != 409 {
			t.Fatalf("%s: %d", path, got)
		}
	}
	for _, path := range []string{"/api/admin/campaigns/c/draws", "/api/admin/campaigns/c/stats", "/api/admin/campaigns/c/export.csv", "/api/admin/offers/o"} {
		if got := request(h, "GET", path, "", "admin").Code; got != 404 {
			t.Fatalf("%s: %d", path, got)
		}
	}
}
func TestCSVFormulaEscaping(t *testing.T) {
	for _, value := range []string{"=cmd", " +1", "-1", "@SUM(A1)", "\t=1"} {
		if !strings.HasPrefix(csvCell(value), "'") {
			t.Fatalf("unsafe cell %q", value)
		}
	}
	if csvCell("") != "" || csvCell("normal") != "normal" {
		t.Fatal("ordinary cell changed")
	}
}
