//go:build integration

package app_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"promo/internal/transport/httpapi/generated"
)

func TestMAXEndToEndIgnoredUpdatesPreservePrizeClaim(t *testing.T) {
	e := newMAXE2E(t)
	h := e.h
	ctx := context.Background()
	for _, user := range h.run.UserIDs[:3] {
		e.provider.setMembership(user, "yes")
		start := len(e.provider.sent())
		e.callback(user, "check")
		e.message(start, user, func(m maxWireMessage) bool {
			return strings.Contains(m.Text, "Заявка зарегистрирована")
		})
	}
	day, err := h.store.CurrentDay(ctx, h.run.CampaignID, h.clock.Now())
	must(t, err)
	h.clock.Set(day.EndsAt)
	drawPath := "/api/admin/campaigns/" + h.run.CampaignID + "/draws/" + h.run.DrawID
	var draw generated.Draw
	e.request(http.MethodPost, drawPath+"/run", nil, &draw)
	e.request(http.MethodPost, drawPath+"/approve", nil, nil)
	user := draw.Selection.Winners[0]
	prize := e.message(0, user, func(m maxWireMessage) bool { return m.callback("claim_begin:") != "" })
	start := len(e.provider.sent())
	e.callback(user, prize.callback("claim_begin:"))
	e.message(start, user, func(m maxWireMessage) bool { return strings.Contains(m.Text, "имя и фамилию") })
	start = len(e.provider.sent())
	e.text(user, "claim-name", "Иван Петров")
	consent := e.message(start, user, func(m maxWireMessage) bool { return m.callback("claim:") != "" })

	pending := func() [2]string {
		var offer, name string
		must(t, h.observer.QueryRow(ctx, "SELECT offer_id,name FROM "+pgx.Identifier{h.name, "pending_claims"}.Sanitize()+" WHERE user_id=$1", user).Scan(&offer, &name))
		return [2]string{offer, name}
	}
	queueSize := func() [2]int {
		var inbox, outbox int
		must(t, h.observer.QueryRow(ctx, "SELECT (SELECT count(*) FROM "+pgx.Identifier{h.name, "inbox"}.Sanitize()+"),(SELECT count(*) FROM "+pgx.Identifier{h.name, "outbox"}.Sanitize()+")").Scan(&inbox, &outbox))
		return [2]int{inbox, outbox}
	}
	providerCalls := func() [3]int {
		e.provider.mu.Lock()
		defer e.provider.mu.Unlock()
		checks := 0
		for _, calls := range e.provider.checks {
			checks += len(calls)
		}
		return [3]int{checks, len(e.provider.messages), len(e.provider.answers)}
	}
	beforePending, beforeQueue, beforeOffers := pending(), queueSize(), e.offers()
	beforeActivities, err := h.store.Activities(ctx, h.run.CampaignID, user)
	must(t, err)
	beforeProvider := providerCalls()
	for _, test := range []struct {
		name    string
		body    any
		forward bool
	}{
		{name: "photo without caption", body: map[string]any{"mid": "photo-null", "text": nil, "attachments": []any{map[string]string{"type": "image"}}}},
		{name: "photo without text field", body: map[string]any{"mid": "photo-omitted", "attachments": []any{map[string]string{"type": "image"}}}},
		{name: "empty text", body: map[string]string{"mid": "empty", "text": ""}},
		{name: "forward", body: nil, forward: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scoped := *e
			scoped.t = t
			message := map[string]any{
				"sender": map[string]int64{"user_id": user}, "recipient": map[string]string{"chat_type": "dialog"}, "body": test.body,
			}
			if test.forward {
				message["link"] = map[string]any{"type": "forward", "message": map[string]string{"mid": "forwarded-command", "text": "/participate"}}
			}
			var ack generated.WebhookAck
			scoped.request(http.MethodPost, "/webhooks/max", map[string]any{"update_type": "message_created", "timestamp": h.clock.Now().UnixMilli(), "message": message}, &ack)
			if !ack.Accepted || !ack.Ignored || ack.EventID != "" {
				t.Fatalf("non-text event was not ignored: %+v", ack)
			}
		})
	}
	for _, source := range []string{"null", "omitted"} {
		for _, payload := range []string{"promo", "check", "participate", prize.callback("claim_begin:"), consent.callback("claim:")} {
			t.Run(source+" message/"+payload, func(t *testing.T) {
				scoped := *e
				scoped.t = t
				update := map[string]any{
					"update_type": "message_callback", "timestamp": h.clock.Now().UnixMilli(),
					"callback": map[string]any{
						"callback_id": "deleted-message:" + source + ":" + payload, "payload": payload,
						"user": map[string]int64{"user_id": user},
					},
				}
				if source == "null" {
					update["message"] = nil
				}
				for attempt := 0; attempt < 2; attempt++ {
					var ack generated.WebhookAck
					scoped.request(http.MethodPost, "/webhooks/max", update, &ack)
					if !ack.Accepted || !ack.Ignored || ack.EventID != "" || ack.Duplicate {
						t.Fatalf("callback without its message was not ignored: attempt=%d ack=%+v", attempt, ack)
					}
				}
			})
		}
	}
	e.idle()
	afterActivities, err := h.store.Activities(ctx, h.run.CampaignID, user)
	must(t, err)
	if pending() != beforePending || queueSize() != beforeQueue || providerCalls() != beforeProvider || !reflect.DeepEqual(e.offers(), beforeOffers) || !reflect.DeepEqual(afterActivities, beforeActivities) {
		t.Fatal("ignored updates changed the pending claim, chances, offers, message queues or MAX requests")
	}
	start = len(e.provider.sent())
	e.callback(user, consent.callback("claim:"))
	e.message(start, user, func(m maxWireMessage) bool {
		return strings.Contains(m.Text, "Получение приза подтверждено")
	})
	for _, offer := range e.offers() {
		if offer.UserID == user && (offer.Status != "claimed" || offer.Name != "Иван Петров") {
			t.Fatalf("saved name was not preserved for consent: %+v", offer)
		}
	}
}
