package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"promo/internal/platform/demo"
	"promo/internal/transport/httpapi/generated"
)

func TestMAXNonTextMessagesAreAcknowledgedWithoutCommands(t *testing.T) {
	for _, test := range []struct {
		name, content string
	}{
		{"photo without caption", `"body":{"mid":"photo-1","text":null,"attachments":[{"type":"image","payload":{"url":"https://example.org/photo.jpg"}}]}`},
		{"photo without text field", `"body":{"mid":"photo-1","attachments":[{"type":"image"}]}`},
		{"empty caption", `"body":{"mid":"photo-1","text":""}`},
		{"blank caption", `"body":{"mid":"photo-1","text":" \n\t "}`},
		{"forward", `"body":null,"link":{"type":"forward","message":{"mid":"forwarded-1","text":"/participate"}}`},
		{"message without body", `"link":{"type":"forward","message":{"mid":"forwarded-1","text":"/participate"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &repositoryStub{}
			h := handler(repo, &serviceStub{}, "production")
			body := fmt.Sprintf(`{"update_type":"message_created","timestamp":1,"message":{"sender":{"user_id":42},"recipient":{"chat_type":"dialog"},%s}}`, test.content)
			response := request(h, http.MethodPost, "/webhooks/max", body, "")
			var ack generated.WebhookAck
			if err := json.Unmarshal(response.Body.Bytes(), &ack); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || !ack.Accepted || !ack.Ignored || ack.EventID != "" || repo.command.ID != "" {
				t.Fatalf("non-text message created a command: status=%d response=%s command=%+v", response.Code, response.Body, repo.command)
			}
		})
	}
}

func TestMAXPhotoCaptionCreatesTheTextCommand(t *testing.T) {
	repo := &repositoryStub{}
	response := request(handler(repo, &serviceStub{}, "production"), http.MethodPost, "/webhooks/max",
		`{"update_type":"message_created","timestamp":1,"message":{"sender":{"user_id":42},"recipient":{"chat_type":"dialog"},"body":{"mid":"photo-1","text":" /participate ","attachments":[{"type":"image"}]}}}`, "")
	if response.Code != http.StatusOK || repo.command.Action != "participate" || repo.command.UserID != 42 {
		t.Fatalf("caption was not accepted: status=%d response=%s command=%+v", response.Code, response.Body, repo.command)
	}
}

func TestMAXNonTextMessagesStillValidateIdentity(t *testing.T) {
	for _, body := range []string{
		`{"sender":{"user_id":0},"recipient":{"chat_type":"dialog"},"body":null}`,
		`{"recipient":{"chat_type":"dialog"},"body":null}`,
		`{"sender":{"user_id":42},"recipient":{"chat_type":"dialog"},"body":{"text":null}}`,
		`{"sender":{"user_id":42},"recipient":{"chat_type":"dialog"},"body":{"mid":"","text":null}}`,
	} {
		repo := &repositoryStub{}
		response := request(handler(repo, &serviceStub{}, "production"), http.MethodPost, "/webhooks/max",
			`{"update_type":"message_created","timestamp":1,"message":`+body+`}`, "")
		if response.Code != http.StatusBadRequest || repo.command.ID != "" {
			t.Fatalf("invalid message was accepted: status=%d response=%s command=%+v", response.Code, response.Body, repo.command)
		}
	}
}

func TestMAXCallbackWithNullMessageBodyIsAcknowledged(t *testing.T) {
	repo := &repositoryStub{}
	calls := 0
	responder := callbackResponderFunc(func(_ context.Context, userID int64, id string) error {
		calls++
		if userID != 42 || id != "callback-1" || repo.command.Action != "participate" {
			t.Fatalf("callback acknowledgement lost its identity or preceded acceptance: user=%d id=%q command=%+v", userID, id, repo.command)
		}
		return nil
	})
	h := New(&serviceStub{}, repo, demo.New(), responder, Config{Mode: "production", CampaignID: "campaign", WebhookSecret: "secret"})
	response := request(h, http.MethodPost, "/webhooks/max",
		`{"update_type":"message_callback","timestamp":1,"message":{"recipient":{"chat_type":"dialog"},"body":null},"callback":{"callback_id":"callback-1","user":{"user_id":42},"payload":"participate"}}`, "")
	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("callback was not acknowledged: status=%d calls=%d response=%s", response.Code, calls, response.Body)
	}
}
