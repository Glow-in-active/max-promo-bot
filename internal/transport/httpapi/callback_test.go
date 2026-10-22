package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"promo/internal/platform/demo"
	"promo/internal/platform/maxapi"
	"promo/internal/transport/httpapi/generated"
)

type callbackResponderFunc func(context.Context, int64, string) error

func (f callbackResponderFunc) AnswerCallback(ctx context.Context, userID int64, id string) error {
	return f(ctx, userID, id)
}

const callbackBody = `{"update_type":"message_callback","timestamp":1,"message":{"recipient":{"chat_type":"dialog"}},"callback":{"callback_id":"original-callback-id","user":{"user_id":42},"payload":"promo"}}`

func TestCallbacksWithoutMessagesAreIgnored(t *testing.T) {
	_, router := contractRouter(t)
	for _, mode := range []string{"demo", "simulator", "production"} {
		for _, message := range []struct{ name, field string }{
			{"deleted", `"message":null,`},
			{"absent", ""},
		} {
			for _, payload := range []string{"promo", "participate", "claim_begin:offer", "claim:offer", "unknown", ""} {
				t.Run(mode+"/"+message.name+"/"+payload, func(t *testing.T) {
					repo := &repositoryStub{error: errors.New("storage unavailable")}
					calls := 0
					responder := callbackResponderFunc(func(context.Context, int64, string) error { calls++; return nil })
					h := New(&serviceStub{}, repo, demo.New(), responder, Config{Mode: mode, CampaignID: "campaign", WebhookSecret: "secret"})
					body := fmt.Sprintf(`{"update_type":"message_callback","timestamp":1,%s"callback":{"callback_id":"deleted-message-callback","user":{"user_id":42},"payload":%q}}`, message.field, payload)
					for range 2 {
						response := request(h, http.MethodPost, "/webhooks/max", body, "")
						var ack generated.WebhookAck
						if err := json.Unmarshal(response.Body.Bytes(), &ack); err != nil {
							t.Fatal(err)
						}
						if response.Code != http.StatusOK || !ack.Accepted || !ack.Ignored || ack.EventID != "" || ack.Duplicate || repo.command.ID != "" || calls != 0 {
							t.Fatalf("callback without its message was processed: status=%d response=%s command=%+v answers=%d", response.Code, response.Body, repo.command, calls)
						}
						assertResponseContract(t, router, http.MethodPost, "/webhooks/max", response)
					}
				})
			}
		}
	}
}

func TestCallbacksWithoutMessagesStillValidateTheRequest(t *testing.T) {
	for _, test := range []struct {
		name, message, timestamp, secret string
		status                           int
	}{
		{"number", "42", "1", "secret", http.StatusBadRequest},
		{"string", `"deleted"`, "1", "secret", http.StatusBadRequest},
		{"array", "[]", "1", "secret", http.StatusBadRequest},
		{"boolean", "false", "1", "secret", http.StatusBadRequest},
		{"invalid timestamp", "null", "0", "secret", http.StatusBadRequest},
		{"unauthorized", "null", "1", "incorrect-secret", http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &repositoryStub{}
			calls := 0
			responder := callbackResponderFunc(func(context.Context, int64, string) error { calls++; return nil })
			h := New(&serviceStub{}, repo, demo.New(), responder, Config{Mode: "production", CampaignID: "campaign", WebhookSecret: "secret"})
			body := fmt.Sprintf(`{"update_type":"message_callback","timestamp":%s,"message":%s,"callback":{"callback_id":"deleted-message-callback","user":{"user_id":42},"payload":"promo"}}`, test.timestamp, test.message)
			r := httptest.NewRequest(http.MethodPost, "/webhooks/max", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Max-Bot-Api-Secret", test.secret)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, r)
			if response.Code != test.status || repo.command.ID != "" || calls != 0 {
				t.Fatalf("request validation changed: status=%d response=%s command=%+v answers=%d", response.Code, response.Body, repo.command, calls)
			}
		})
	}
}

func TestCallbackAcknowledgementFollowsDurableAcceptance(t *testing.T) {
	for _, ackError := range []error{nil, errors.New("simulated acknowledgement failure")} {
		repo := &repositoryStub{}
		calls := 0
		responder := callbackResponderFunc(func(ctx context.Context, userID int64, id string) error {
			calls++
			if repo.command.ID == "" {
				t.Fatal("acknowledgement preceded durable inbox acceptance")
			}
			if id != "original-callback-id" || userID != 42 {
				t.Fatalf("platform callback ID was replaced: %q", id)
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 750*time.Millisecond {
				t.Fatal("acknowledgement is not bounded")
			}
			return ackError
		})
		h := New(&serviceStub{}, repo, demo.New(), responder, Config{Mode: "production", CampaignID: "campaign", WebhookSecret: "secret"})
		response := request(h, "POST", "/webhooks/max", callbackBody, "")
		if response.Code != 200 || calls != 1 || repo.command.Action != "promo" {
			t.Fatalf("accepted event was affected by acknowledgement: %d, calls=%d", response.Code, calls)
		}
	}
}

func TestUnacceptedOrNonCallbackUpdatesNeverAcknowledge(t *testing.T) {
	for _, test := range []struct {
		name, body                   string
		unauthorized, storageFailure bool
		status                       int
	}{
		{name: "noncallback", body: `{"update_type":"bot_started","timestamp":1,"chat_id":1,"user":{"user_id":42}}`, status: 200},
		{name: "unauthorized", body: callbackBody, unauthorized: true, status: 401},
		{name: "invalid", body: strings.Replace(callbackBody, `"payload":"promo"`, `"payload":"unknown"`, 1), status: 400},
		{name: "group", body: strings.Replace(callbackBody, `"chat_type":"dialog"`, `"chat_type":"chat"`, 1), status: 200},
		{name: "storage failure", body: callbackBody, storageFailure: true, status: 500},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &repositoryStub{}
			if test.storageFailure {
				repo.error = errors.New("storage unavailable")
			}
			calls := 0
			responder := callbackResponderFunc(func(context.Context, int64, string) error { calls++; return nil })
			h := New(&serviceStub{}, repo, demo.New(), responder, Config{Mode: "production", CampaignID: "campaign", WebhookSecret: "secret"})
			r := httptest.NewRequest("POST", "/webhooks/max", strings.NewReader(test.body))
			r.Header.Set("Content-Type", "application/json")
			if !test.unauthorized {
				r.Header.Set("X-Max-Bot-Api-Secret", "secret")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.status || calls != 0 {
				t.Fatalf("status=%d, acknowledgement calls=%d", w.Code, calls)
			}
		})
	}
}

func TestProductionCallbackUsesMAXAnswerEndpoint(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/answers" || r.URL.Query().Get("callback_id") != "original-callback-id" {
			t.Error("incorrect MAX callback endpoint")
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer upstream.Close()
	client, err := maxapi.New(maxapi.Config{Token: "test-token", BaseURL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	h := New(&serviceStub{}, &repositoryStub{}, nil, client, Config{Mode: "production", CampaignID: "campaign", WebhookSecret: "secret"})
	if response := request(h, "POST", "/webhooks/max", callbackBody, ""); response.Code != 200 || calls != 1 {
		t.Fatalf("MAX responder was not invoked: status=%d calls=%d", response.Code, calls)
	}
}

func TestCallbackAcknowledgementTimeoutPreservesHTTPAcceptance(t *testing.T) {
	responder := callbackResponderFunc(func(ctx context.Context, _ int64, _ string) error { <-ctx.Done(); return ctx.Err() })
	h := New(&serviceStub{}, &repositoryStub{}, nil, responder, Config{Mode: "production", CampaignID: "campaign", WebhookSecret: "secret"})
	start := time.Now()
	response := request(h, "POST", "/webhooks/max", callbackBody, "")
	if response.Code != 200 || time.Since(start) > 2*time.Second {
		t.Fatalf("acknowledgement timeout changed durable acceptance: %d", response.Code)
	}
}
