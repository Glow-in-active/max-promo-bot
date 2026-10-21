package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"promo/internal/domain"
)

func simulatorHandler(repo *repositoryStub) http.Handler {
	return New(&serviceStub{}, repo, nil, callbackResponderFunc(func(context.Context, int64, string) error { return nil }), Config{
		Mode: "simulator", AdminToken: "admin", DemoToken: "demo", WebhookSecret: "secret", CampaignID: "configured-campaign",
	})
}

func TestSimulatorProvidesAuthenticatedTestCampaigns(t *testing.T) {
	repo := &repositoryStub{run: domain.DemoRun{CampaignID: "fresh"}, messages: []domain.Message{{ID: "message-id", UserID: 42, Text: "hello"}}}
	h := simulatorHandler(repo)
	for _, test := range []struct {
		method, path, body string
		status             int
		contains           string
	}{
		{"POST", "/_demo/runs", `{"scenario":"standard"}`, http.StatusCreated, `"campaign_id":"fresh"`},
		{"GET", "/_demo/runs/c/messages", "", http.StatusOK, `"message-id"`},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := request(h, test.method, test.path, test.body, "demo")
			if response.Code != test.status || !strings.Contains(response.Body.String(), test.contains) {
				t.Fatalf("test campaign response: status=%d body=%s", response.Code, response.Body.String())
			}
			if response := request(h, test.method, test.path, test.body, ""); response.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated test campaign request returned %d", response.Code)
			}
		})
	}
}

func TestSimulatorSubscriptionBelongsToTheExternalAPI(t *testing.T) {
	h := simulatorHandler(&repositoryStub{})
	response := request(h, "POST", "/_demo/runs/c/subscription", `{"user_id":42,"status":"yes"}`, "demo")
	if response.Code != http.StatusNotFound {
		t.Fatalf("simulator subscription override: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSimulatorWebhookUsesTheRequestedTestCampaign(t *testing.T) {
	const body = `{"update_type":"bot_started","timestamp":10,"chat_id":1,"user":{"user_id":42}}`
	repo := &repositoryStub{}
	h := simulatorHandler(repo)
	response := request(h, "POST", "/webhooks/max", body, "")
	if response.Code != http.StatusOK || repo.command.CampaignID != "campaign" {
		t.Fatalf("simulator campaign selection: status=%d command=%+v", response.Code, repo.command)
	}
	for _, test := range []struct {
		name, campaign, secret string
		status                 int
	}{
		{"missing campaign", "", "secret", http.StatusBadRequest},
		{"missing secret", "campaign", "", http.StatusUnauthorized},
		{"wrong secret", "campaign", "wrong", http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/webhooks/max", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Demo-Campaign", test.campaign)
			request.Header.Set("X-Max-Bot-Api-Secret", test.secret)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("simulator webhook: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
