package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"promo/internal/domain"
	"promo/internal/transport/httpapi/generated"
)

func contractRouter(t *testing.T) (*openapi3.T, routers.Router) {
	t.Helper()
	spec, err := generated.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	spec.Servers = nil
	if err = spec.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	router, err := gorillamux.NewRouter(spec)
	if err != nil {
		t.Fatal(err)
	}
	return spec, router
}

func assertResponseContract(t *testing.T, router routers.Router, method, path string, response *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	route, params, err := router.FindRoute(r)
	if err != nil {
		t.Fatal(err)
	}
	options := &openapi3filter.Options{IncludeResponseStatus: true}
	if !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
		options.ExcludeResponseBody = true
		if response.Code == http.StatusOK && response.Body.Len() == 0 {
			t.Fatal("empty CSV/SVG response")
		}
	}
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    r,
			PathParams: params,
			Route:      route,
		},
		Status:  response.Code,
		Header:  response.Header(),
		Body:    io.NopCloser(bytes.NewReader(response.Body.Bytes())),
		Options: options,
	}
	if err = openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		t.Fatalf("%s %s: %v\nbody: %s", method, path, err, response.Body.String())
	}
}

func TestEveryOperationRespondsAccordingToGeneratedContract(t *testing.T) {
	spec, router := contractRouter(t)
	repo := &repositoryStub{
		run: domain.DemoRun{
			ID:          "run",
			CampaignID:  "campaign",
			DayID:       "day",
			DrawID:      "draw",
			FinalDrawID: "final",
			UserIDs:     []int64{42},
			Now:         time.Now(),
		},
		messages: []domain.Message{},
	}
	h := handler(repo, &serviceStub{}, "demo")
	cases := []struct{ method, path, body, token string }{
		{"GET", "/healthz", "", ""},
		{"GET", "/readyz", "", ""},
		{"GET", "/assets/banner.svg", "", ""},
		{"POST", "/webhooks/max", `{"update_type":"bot_started","timestamp":1,"chat_id":1,"user":{"user_id":42}}`, ""},
		{"POST", "/_demo/runs", `{"scenario":"standard"}`, "demo"},
		{"GET", "/_demo/runs/c/messages", "", "demo"},
		{"POST", "/_demo/runs/c/subscription", `{"user_id":42,"status":"yes"}`, "demo"},
		{"GET", "/api/admin/campaigns/c/draws", "", "admin"},
		{"POST", "/api/admin/campaigns/c/draws/d/run", "", "admin"},
		{"POST", "/api/admin/campaigns/c/draws/d/approve", "", "admin"},
		{"GET", "/api/admin/campaigns/c/draws/d/offers", "", "admin"},
		{"GET", "/api/admin/campaigns/c/stats", "", "admin"},
		{"GET", "/api/admin/campaigns/c/export.csv", "", "admin"},
		{"GET", "/api/admin/offers/o", "", "admin"},
		{"POST", "/api/admin/offers/o/retry", "", "admin"},
		{"POST", "/api/admin/offers/o/expire", "", "admin"},
		{"POST", "/api/admin/offers/o/delivered", "", "admin"},
		{"POST", "/api/admin/tick", "", "admin"},
	}
	covered := map[string]bool{}
	for _, test := range cases {
		t.Run(test.method+test.path, func(t *testing.T) {
			response := request(h, test.method, test.path, test.body, test.token)
			if response.Code != 200 && response.Code != 201 {
				t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
			}
			assertResponseContract(t, router, test.method, test.path, response)
			route, _, err := router.FindRoute(httptest.NewRequest(test.method, test.path, nil))
			if err != nil {
				t.Fatal(err)
			}
			covered[route.Operation.OperationID] = true
		})
	}
	for path, item := range spec.Paths.Map() {
		for method, operation := range item.Operations() {
			if !covered[operation.OperationID] {
				t.Errorf("missing response contract test: %s %s (%s)", method, path, operation.OperationID)
			}
		}
	}
}

func TestErrorResponsesFollowGeneratedContract(t *testing.T) {
	_, router := contractRouter(t)
	h := handler(&repositoryStub{}, &serviceStub{}, "demo")
	cases := []struct {
		method, path, body, token string
		status                    int
	}{
		{"POST", "/_demo/runs", `{"scenario":"standard"}`, "", 401},
		{"POST", "/_demo/runs", `{"scenario":"unknown"}`, "demo", 400},
		{"POST", "/_demo/runs", `{"scenario":"standard","extra":true}`, "demo", 400},
		{"POST", "/_demo/runs", `{"scenario":"` + strings.Repeat("x", maxBodyBytes) + `"}`, "demo", 413},
		{"POST", "/_demo/runs/c/subscription", `{"user_id":0,"status":"yes"}`, "demo", 400},
		{"POST", "/_demo/runs/c/subscription", `{"user_id":42,"status":"bad"}`, "demo", 400},
		{"GET", "/_demo/runs/c/messages?after=" + strings.Repeat("x", 201), "", "demo", 400},
	}
	for _, test := range cases {
		response := request(h, test.method, test.path, test.body, test.token)
		if response.Code != test.status {
			t.Fatalf("%s: status %d, expected %d", test.path, response.Code, test.status)
		}
		assertResponseContract(t, router, test.method, test.path, response)
	}
	conflict := handler(&repositoryStub{}, &serviceStub{err: domain.Fail(domain.InvalidTransition, "closed")}, "demo")
	assertResponseContract(t, router, "POST", "/api/admin/campaigns/c/draws/d/run", request(conflict, "POST", "/api/admin/campaigns/c/draws/d/run", "", "admin"))
	broken := handler(&repositoryStub{error: errors.New("private database detail")}, &serviceStub{}, "demo")
	response := request(broken, "GET", "/api/admin/campaigns/c/stats", "", "admin")
	assertResponseContract(t, router, "GET", "/api/admin/campaigns/c/stats", response)
	if strings.Contains(response.Body.String(), "private") {
		t.Fatal("internal error leaked")
	}
}

func TestRequestContractKeepsExternalMAXFieldsExtensible(t *testing.T) {
	h := handler(&repositoryStub{}, &serviceStub{}, "production")
	body := `{"update_type":"message_created","timestamp":1,"future":{},"message":{"sender":{"user_id":42,"future":true},"recipient":{"chat_type":"dialog","future":true},"body":{"mid":"stable-id","text":"/help","future":"field"}}}`
	if response := request(h, "POST", "/webhooks/max", body, ""); response.Code != 200 {
		t.Fatalf("MAX extension rejected: %d %s", response.Code, response.Body.String())
	}
}

func TestGeneratedModelsPreserveEmptyCollectionsAndOptionalTimes(t *testing.T) {
	_, router := contractRouter(t)
	response := httptest.NewRecorder()
	writeJSON(response, 200, drawModel(domain.Draw{ID: "draw", CampaignID: "campaign", Kind: "daily", State: domain.DrawOpen, ClosesAt: time.Now(), Selection: domain.Selection{}}))
	assertResponseContract(t, router, "POST", "/api/admin/campaigns/c/draws/d/run", response)
	if strings.Contains(response.Body.String(), "approved_at") || !strings.Contains(response.Body.String(), `"winners":null`) {
		t.Fatalf("optional times or nil lists changed: %s", response.Body.String())
	}
	response = httptest.NewRecorder()
	writeJSON(response, 200, statsModel(domain.Stats{}))
	assertResponseContract(t, router, "GET", "/api/admin/campaigns/c/stats", response)
	if !strings.Contains(response.Body.String(), `"promo_remaining":[]`) {
		t.Fatal("empty partner stock must be an array")
	}
	now := time.Now().UTC()
	offer := domain.Offer{ID: "offer", SlotID: "slot", DrawID: "draw", UserID: 42, Status: "claimed", SentAt: &now, Deadline: &now, ClaimedAt: &now, ConsentAt: &now, Name: "Иван Иванов", DeliveryState: "delivered"}
	response = httptest.NewRecorder()
	writeJSON(response, 200, offerModel(offer))
	assertResponseContract(t, router, "GET", "/api/admin/offers/o", response)
}

func TestRequestContractRejectsMissingMAXSourceTimeBeforeInbox(t *testing.T) {
	for _, body := range []string{
		strings.Replace(callbackBody, `,"timestamp":1`, "", 1),
		strings.Replace(callbackBody, `"timestamp":1`, `"timestamp":0`, 1),
		strings.Replace(callbackBody, `"timestamp":1`, `"timestamp":-1`, 1),
		strings.Replace(callbackBody, `"timestamp":1`, `"timestamp":253402300800000`, 1),
	} {
		repo := &repositoryStub{}
		response := request(handler(repo, &serviceStub{}, "production"), "POST", "/webhooks/max", body, "")
		if response.Code != 400 || repo.command.ID != "" {
			t.Fatalf("invalid source time reached inbox: status=%d command=%+v", response.Code, repo.command)
		}
	}
}
