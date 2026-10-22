//go:build integration

package app_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"promo/internal/domain"
	"promo/internal/transport/httpapi/generated"
)

func TestMAXEndToEndCampaignLifecycle(t *testing.T) {
	e := newMAXE2E(t)
	day, err := e.h.store.CurrentDay(context.Background(), e.h.run.CampaignID, e.h.clock.Now())
	must(t, err)

	e.assertEmptyCampaign()
	user := e.h.run.UserIDs[0]
	welcome := e.startBot(user)
	e.checkMembership(user, welcome)

	registration := e.registerParticipants()
	e.assertCallbackReplay(registration, day)

	drawn := e.runDailyDraw(day)
	prizes := e.approveDailyDraw(drawn)
	winner := drawn.Selection.Winners[0]
	claimed, expiring := e.claimWinner(winner, prizes[winner])

	e.replaceExpiredWinner(drawn, claimed, expiring)
	e.assertDrawSnapshotAndNotifications(drawn)
}

func (e *maxE2E) assertEmptyCampaign() {
	t := e.t
	t.Helper()
	h := e.h
	ctx := context.Background()
	for _, user := range h.run.UserIDs {
		active, err := h.store.Activities(ctx, h.run.CampaignID, user)
		must(t, err)
		if len(active) != 0 {
			t.Fatal("E2E must start without granted chances")
		}
	}
	if len(e.offers()) != 0 {
		t.Fatal("E2E must start without prize offers")
	}
}

func (e *maxE2E) startBot(user int64) maxWireMessage {
	t := e.t
	t.Helper()
	h := e.h
	e.request(http.MethodPost, "/webhooks/max", map[string]any{
		"update_type": "bot_started",
		"timestamp":   h.clock.Now().UnixMilli(),
		"chat_id":     user,
		"user":        map[string]int64{"user_id": user},
	}, nil)
	welcome := e.message(0, user, func(m maxWireMessage) bool { return m.callback("participate") == "participate" })
	banner := false
	for _, attachment := range welcome.Attachments {
		if attachment.Type == "image" && attachment.Payload.URL == "https://example.org/e2e-banner.png" {
			banner = true
		}
	}
	if !banner {
		t.Fatal("bot start did not send a real MAX image attachment")
	}
	return welcome
}

func (e *maxE2E) checkMembership(user int64, welcome maxWireMessage) {
	t := e.t
	t.Helper()
	h := e.h
	ctx := context.Background()
	start := len(e.provider.sent())
	e.callback(user, welcome.callback("participate"))
	participation := e.message(start, user, func(m maxWireMessage) bool { return m.callback("check") == "check" })
	for _, status := range []string{"no", "error"} {
		e.provider.setMembership(user, status)
		start = len(e.provider.sent())
		e.callback(user, participation.callback("check"))
		reply := e.message(start, user, func(m maxWireMessage) bool { return m.Text != "" })
		active, err := h.store.Activities(ctx, h.run.CampaignID, user)
		must(t, err)
		var stats generated.Stats
		e.request(http.MethodGet, "/api/admin/campaigns/"+h.run.CampaignID+"/stats", nil, &stats)
		if len(active) != 0 || stats.Entries[h.run.DrawID] != 0 {
			t.Fatalf("%s membership granted a chance or entry: %+v", status, stats)
		}
		if !strings.Contains(reply.Text, "подписку") {
			t.Fatalf("%s membership did not explain the failed subscription check: %s", status, reply.Text)
		}
	}
}

func (e *maxE2E) registerParticipants() generated.WebhookAck {
	t := e.t
	t.Helper()
	h := e.h
	var lastRegistration generated.WebhookAck
	for _, participant := range h.run.UserIDs[:3] {
		e.provider.setMembership(participant, "yes")
		start := len(e.provider.sent())
		lastRegistration = e.callback(participant, "check")
		e.message(start, participant, func(m maxWireMessage) bool {
			return strings.Contains(m.Text, "Заявка зарегистрирована") && strings.Contains(m.Text, "Накоплено шансов: 1 из 10")
		})
	}
	return lastRegistration
}

func (e *maxE2E) assertCallbackReplay(lastRegistration generated.WebhookAck, day domain.Day) {
	t := e.t
	t.Helper()
	h := e.h
	ctx := context.Background()
	start := len(e.provider.sent())
	beforeReplay, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	e.sequence--
	replayedUser := h.run.UserIDs[2]
	replayedCallback := e.callback(replayedUser, "check")
	e.idle()
	afterReplay, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if !replayedCallback.Duplicate || replayedCallback.EventID != lastRegistration.EventID ||
		len(e.provider.sent()) != start || !reflect.DeepEqual(beforeReplay, afterReplay) {
		t.Fatalf("exact callback replay created inbox/outbox work: first=%+v repeated=%+v messages=%d/%d",
			lastRegistration, replayedCallback, len(beforeReplay), len(afterReplay))
	}
	freshCallback := e.callback(replayedUser, "check")
	if freshCallback.Duplicate || freshCallback.EventID == lastRegistration.EventID {
		t.Fatalf("another button click was discarded as a replay: %+v", freshCallback)
	}
	e.message(start, replayedUser, func(m maxWireMessage) bool { return strings.Contains(m.Text, "уже участвуете") })
	for _, participant := range h.run.UserIDs[:3] {
		active, err := h.store.Activities(ctx, h.run.CampaignID, participant)
		must(t, err)
		if len(active) != 1 || active[0].DayID != day.ID {
			t.Fatalf("callback replay changed chances: %+v", active)
		}
	}
	e.provider.mu.Lock()
	for _, paths := range e.provider.checks {
		for _, path := range paths {
			if path != fmt.Sprintf("/chats/%d/members", day.ChannelID) {
				t.Errorf("wrong partner checked: %s", path)
			}
		}
	}
	e.provider.mu.Unlock()
}

func (e *maxE2E) runDailyDraw(day domain.Day) generated.Draw {
	t := e.t
	t.Helper()
	h := e.h
	h.clock.Set(day.EndsAt)
	var drawn generated.Draw
	drawPath := "/api/admin/campaigns/" + h.run.CampaignID + "/draws/" + h.run.DrawID
	e.request(http.MethodPost, drawPath+"/run", nil, &drawn)
	if len(drawn.Candidates) != 3 || len(drawn.Selection.Winners) != 2 || len(drawn.Selection.Reserve) != 1 {
		t.Fatalf("draw did not freeze the webhook registrations: %+v", drawn)
	}
	for _, candidate := range drawn.Candidates {
		if candidate.Weight != 1 {
			t.Fatalf("callback replay inflated snapshot weight: %+v", candidate)
		}
	}
	e.idle()
	for _, message := range e.provider.sent() {
		if message.callback("claim_begin:") != "" {
			t.Fatal("prize was sent before approval")
		}
	}
	if len(e.offers()) != 0 {
		t.Fatal("prize offer was created before approval")
	}
	return drawn
}

func (e *maxE2E) approveDailyDraw(drawn generated.Draw) map[int64]maxWireMessage {
	t := e.t
	t.Helper()
	drawPath := "/api/admin/campaigns/" + e.h.run.CampaignID + "/draws/" + drawn.ID
	e.request(http.MethodPost, drawPath+"/approve", nil, nil)
	prizes := map[int64]maxWireMessage{}
	for _, winner := range drawn.Selection.Winners {
		prizes[winner] = e.message(0, winner, func(m maxWireMessage) bool { return m.callback("claim_begin:") != "" })
	}
	return prizes
}

func (e *maxE2E) claimWinner(winner int64, prize maxWireMessage) (generated.Offer, generated.Offer) {
	t := e.t
	t.Helper()
	offers := e.offers()
	if len(offers) != 2 {
		t.Fatalf("approval created %d offers", len(offers))
	}
	var claimedID, expiredID string
	for _, offer := range offers {
		if offer.Status != "notified" || offer.SentAt.IsZero() || !offer.Deadline.Equal(offer.SentAt.Add(time.Hour)) {
			t.Fatalf("invalid delivery deadline: %+v", offer)
		}
		if offer.UserID == winner {
			claimedID = offer.ID
		} else {
			expiredID = offer.ID
		}
	}
	consent := e.submitWinnerName(winner, claimedID, prize)
	start := len(e.provider.sent())
	e.callback(winner, consent.callback("claim:"))
	e.message(start, winner, func(m maxWireMessage) bool {
		return strings.Contains(m.Text, "Получение приза подтверждено")
	})
	var originalClaim generated.Offer
	var expiring generated.Offer
	for _, offer := range e.offers() {
		if offer.ID == claimedID {
			originalClaim = offer
			if offer.Status != "claimed" || offer.Name != "Иван Петров" || offer.ConsentAt.IsZero() || offer.ClaimedAt.IsZero() {
				t.Fatalf("name and consent were not persisted: %+v", offer)
			}
		}
		if offer.ID == expiredID {
			expiring = offer
		}
	}
	start = len(e.provider.sent())
	e.callback(winner, consent.callback("claim:"))
	e.message(start, winner, func(m maxWireMessage) bool { return strings.Contains(m.Text, "уже подтверждено") })
	return originalClaim, expiring
}

func (e *maxE2E) submitWinnerName(winner int64, claimedID string, prize maxWireMessage) maxWireMessage {
	t := e.t
	t.Helper()
	start := len(e.provider.sent())
	e.callback(winner, prize.callback("claim_begin:"))
	e.message(start, winner, func(m maxWireMessage) bool { return strings.Contains(m.Text, "имя и фамилию") })
	start = len(e.provider.sent())
	ack := e.text(winner, "claim-name-1", "Иван Петров")
	if !ack.Accepted || ack.Duplicate {
		t.Fatalf("name not accepted: %+v", ack)
	}
	consent := e.message(start, winner, func(m maxWireMessage) bool { return m.callback("claim:") == "claim:"+claimedID })
	before := len(e.provider.sent())
	replayed := e.text(winner, "claim-name-1", "Иван Петров")
	e.idle()
	if !replayed.Duplicate || len(e.provider.sent()) != before {
		t.Fatal("replayed message MID created another response")
	}
	for _, offer := range e.offers() {
		if offer.ID == claimedID && (offer.Status == "claimed" || !offer.ConsentAt.IsZero()) {
			t.Fatal("name alone claimed a prize without consent")
		}
	}
	return consent
}

func (e *maxE2E) replaceExpiredWinner(drawn generated.Draw, claimed, expiring generated.Offer) {
	t := e.t
	t.Helper()
	h := e.h
	deadline := expiring.Deadline
	drawPath := "/api/admin/campaigns/" + h.run.CampaignID + "/draws/" + drawn.ID
	h.clock.Set(deadline.Add(-time.Microsecond))
	e.request(http.MethodPost, "/api/admin/tick", nil, nil)
	if len(e.offers()) != 2 {
		t.Fatal("reserve replaced winner before the deadline")
	}
	h.clock.Set(deadline)
	e.request(http.MethodPost, "/api/admin/tick", nil, nil)
	reserve := drawn.Selection.Reserve[0]
	reserveMessage := e.message(0, reserve, func(m maxWireMessage) bool { return m.callback("claim_begin:") != "" })
	e.request(http.MethodPost, "/api/admin/tick", nil, nil)
	e.request(http.MethodPost, drawPath+"/approve", nil, nil)
	e.idle()
	offers := e.offers()
	if len(offers) != 3 {
		t.Fatalf("expiry or approval replay duplicated replacement: %+v", offers)
	}
	for _, offer := range offers {
		switch offer.ID {
		case claimed.ID:
			if !reflect.DeepEqual(offer, claimed) {
				t.Fatalf("expiry changed the claimed winner: %+v", offer)
			}
		case expiring.ID:
			if offer.Status != "expired" {
				t.Fatalf("unclaimed winner did not expire at deadline: %+v", offer)
			}
		default:
			if offer.UserID != reserve || offer.Status != "notified" ||
				reserveMessage.callback("claim_begin:") != "claim_begin:"+offer.ID ||
				!offer.Deadline.Equal(deadline.Add(time.Hour)) {
				t.Fatalf("replacement did not receive its own full deadline: %+v", offer)
			}
		}
	}
}

func (e *maxE2E) assertDrawSnapshotAndNotifications(drawn generated.Draw) {
	t := e.t
	t.Helper()
	h := e.h
	var draws generated.DrawList
	e.request(http.MethodGet, "/api/admin/campaigns/"+h.run.CampaignID+"/draws", nil, &draws)
	found := false
	for _, draw := range draws.Draws {
		if draw.ID == drawn.ID {
			found = true
			if !reflect.DeepEqual(draw.Candidates, drawn.Candidates) || !reflect.DeepEqual(draw.Selection, drawn.Selection) {
				t.Fatal("replacement rewrote the original draw snapshot")
			}
		}
	}
	if !found {
		t.Fatal("approved draw disappeared from the admin API")
	}
	prizeCounts := map[int64]int{}
	for _, message := range e.provider.sent() {
		if message.callback("claim_begin:") != "" {
			prizeCounts[message.UserID]++
		}
	}
	if len(prizeCounts) != 3 {
		t.Fatalf("unexpected prize recipients: %v", prizeCounts)
	}
	for _, count := range prizeCounts {
		if count != 1 {
			t.Fatalf("duplicate prize notifications: %v", prizeCounts)
		}
	}
}
