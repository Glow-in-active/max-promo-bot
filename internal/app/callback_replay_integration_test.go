//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"promo/internal/transport/httpapi/generated"
)

func TestReplayedClaimCallbackCannotReplaceAnotherPrizeConversation(t *testing.T) {
	h := setup(t, "drawable")
	ctx := context.Background()
	campaign, err := h.store.Campaign(ctx, h.run.CampaignID)
	must(t, err)
	h.clock.Set(campaign.EndsAt)
	for _, drawID := range []string{h.run.DrawID, h.run.FinalDrawID} {
		_, err := h.service.RunDraw(ctx, h.run.CampaignID, drawID, "test-admin")
		must(t, err)
		_, err = h.service.ApproveDraw(ctx, h.run.CampaignID, drawID, "test-admin")
		must(t, err)
	}
	first := h.offers(t, h.run.DrawID)[0]
	var secondID string
	for _, offer := range h.offers(t, h.run.FinalDrawID) {
		if offer.UserID == first.UserID {
			secondID = offer.ID
		}
	}
	if secondID == "" || first.UserID != h.run.UserIDs[0] {
		t.Fatal("fixture must give the same participant two current prizes")
	}
	handler := sourceCallbackHandler(h)
	post := func(body map[string]any) generated.WebhookAck {
		t.Helper()
		data, err := json.Marshal(body)
		must(t, err)
		request := httptest.NewRequest(http.MethodPost, "/webhooks/max", strings.NewReader(string(data)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Max-Bot-Api-Secret", "test-secret")
		request.Header.Set("X-Demo-Campaign", h.run.CampaignID)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("webhook HTTP %d: %s", response.Code, response.Body.String())
		}
		var ack generated.WebhookAck
		must(t, json.Unmarshal(response.Body.Bytes(), &ack))
		if !ack.Accepted || ack.EventID == "" {
			t.Fatalf("webhook was not durably accepted: %+v", ack)
		}
		return ack
	}
	callback := func(payload string) map[string]any {
		return map[string]any{
			"update_type": "message_callback", "timestamp": h.clock.Now().UnixMilli(),
			"message":  map[string]any{"recipient": map[string]string{"chat_type": "dialog"}},
			"callback": map[string]any{"callback_id": "same-platform-button-id", "payload": payload, "user": map[string]int64{"user_id": first.UserID}},
		}
	}
	original := callback("claim_begin:" + first.ID)
	accepted := post(original)
	executeSourceEvent(t, h)
	post(callback("claim_begin:" + secondID))
	executeSourceEvent(t, h)
	post(map[string]any{
		"update_type": "message_created", "timestamp": h.clock.Now().UnixMilli(),
		"message": map[string]any{
			"sender":    map[string]int64{"user_id": first.UserID},
			"recipient": map[string]string{"chat_type": "dialog"},
			"body":      map[string]string{"mid": "second-prize-name", "text": "Иван Петров"},
		},
	})
	consent := executeSourceEvent(t, h)
	if len(consent.Buttons) != 1 || consent.Buttons[0].Payload != "claim:"+secondID {
		t.Fatalf("name did not belong to the second prize: %+v", consent)
	}
	before, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	replayed := post(original)
	event, err := h.store.TakeInbox(ctx, time.Now().Add(time.Second))
	must(t, err)
	if event != nil {
		h.execute(t, event.Command)
	}
	after, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	post(callback(consent.Buttons[0].Payload))
	executeSourceEvent(t, h)
	claimed, err := h.store.Offer(ctx, secondID)
	must(t, err)
	if claimed.Status != "claimed" || claimed.Name != "Иван Петров" || claimed.ConsentAt == nil {
		t.Fatalf("replaying the first prize callback erased the second prize conversation: %+v", claimed)
	}
	unclaimed, err := h.store.Offer(ctx, first.ID)
	must(t, err)
	if unclaimed.Status == "claimed" || unclaimed.ConsentAt != nil || unclaimed.Name != "" {
		t.Fatalf("consent for the second prize claimed the first: %+v", unclaimed)
	}
	if !replayed.Duplicate || replayed.EventID != accepted.EventID || event != nil || len(after) != len(before) {
		t.Fatalf("exact callback replay created inbox/outbox work: first=%+v replay=%+v event=%+v messages=%d/%d", accepted, replayed, event, len(before), len(after))
	}
}
