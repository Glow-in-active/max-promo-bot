//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
	"promo/internal/transport/httpapi"
	"promo/internal/transport/httpapi/generated"
)

func postSourceCallback(t *testing.T, h *harness, handler http.Handler, occurredAt time.Time, callbackID, action string) generated.WebhookAck {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"update_type": "message_callback", "timestamp": occurredAt.UnixMilli(),
		"message":  map[string]any{"recipient": map[string]string{"chat_type": "dialog"}},
		"callback": map[string]any{"callback_id": callbackID, "payload": action, "user": map[string]any{"user_id": h.run.UserIDs[0]}},
	})
	must(t, err)
	request := httptest.NewRequest(http.MethodPost, "/webhooks/max", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Max-Bot-Api-Secret", "test-secret")
	request.Header.Set("X-Demo-Campaign", h.run.CampaignID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("callback HTTP %d: %s", response.Code, response.Body.String())
	}
	var ack generated.WebhookAck
	must(t, json.Unmarshal(response.Body.Bytes(), &ack))
	if !ack.Accepted || ack.EventID == "" {
		t.Fatalf("callback was not durably accepted: %+v", ack)
	}
	return ack
}

func assertSourceCallbackReplay(t *testing.T, h *harness, handler http.Handler, occurredAt time.Time, callbackID, action, eventID string) {
	t.Helper()
	ctx := context.Background()
	before, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	ack := postSourceCallback(t, h, handler, occurredAt, callbackID, action)
	event, err := h.store.TakeInbox(ctx, time.Now().Add(time.Second))
	must(t, err)
	after, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if !ack.Duplicate || ack.EventID != eventID || event != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("exact callback replay created inbox/outbox work: ack=%+v event=%+v messages=%d/%d", ack, event, len(before), len(after))
	}
}

func sourceCallbackHandler(h *harness) http.Handler {
	return httpapi.New(h.service, h.store, h.messenger, h.messenger, httpapi.Config{Mode: "demo", WebhookSecret: "test-secret"})
}

func executeSourceEvent(t *testing.T, h *harness) domain.Message {
	t.Helper()
	event, err := h.store.TakeInbox(context.Background(), time.Now().Add(time.Second))
	must(t, err)
	if event == nil {
		t.Fatal("accepted callback was not queued")
	}
	if event.Command.OccurredAt.IsZero() {
		t.Fatal("MAX source timestamp was not persisted")
	}
	reply := h.execute(t, event.Command)
	must(t, h.store.Within(context.Background(), func(tx app.Tx) error {
		done, err := tx.InboxDone(context.Background(), event.Command.ID)
		if err == nil && !done {
			t.Fatal("executed callback remained eligible for another inbox execution")
		}
		return err
	}))
	messages, err := h.store.Messages(context.Background(), h.run.CampaignID, "")
	must(t, err)
	for _, saved := range messages {
		if saved.ID == reply.ID && saved.Text == reply.Text && saved.UserID == event.Command.UserID && saved.CampaignID == event.Command.CampaignID {
			return reply
		}
	}
	t.Fatalf("callback reply was not durably queued: %+v", reply)
	return domain.Message{}
}

func assertIssuedDays(t *testing.T, h *harness, expected ...string) {
	t.Helper()
	stats, err := h.store.Stats(context.Background(), h.run.CampaignID)
	must(t, err)
	active, err := h.store.Activities(context.Background(), h.run.CampaignID, h.run.UserIDs[0])
	must(t, err)
	if stats.PromoIssued != len(expected) || len(active) != len(expected) {
		t.Fatalf("wrong grants: codes=%d activities=%+v", stats.PromoIssued, active)
	}
	for i, dayID := range expected {
		if active[i].DayID != dayID {
			t.Fatalf("activity moved to another day: %+v", active)
		}
	}
}

func TestReplayedCallbackRemainsBoundToItsOriginalDay(t *testing.T) {
	h := setup(t, "standard")
	handler := sourceCallbackHandler(h)
	source := h.clock.Now()
	firstDay, err := h.store.CurrentDay(context.Background(), h.run.CampaignID, source)
	must(t, err)
	source = firstDay.EndsAt.Add(-time.Millisecond)
	h.clock.Set(source)
	original := postSourceCallback(t, h, handler, source, "original-button", "promo")
	first := executeSourceEvent(t, h)
	if !strings.Contains(first.Text, "Промокод: DEMO-") {
		t.Fatalf("first code missing: %s", first.Text)
	}
	h.clock.Set(firstDay.EndsAt)
	assertSourceCallbackReplay(t, h, handler, source, "original-button", "promo", original.EventID)
	assertIssuedDays(t, h, firstDay.ID)
	postSourceCallback(t, h, handler, source, "new-click-with-old-time", "promo")
	replay := executeSourceEvent(t, h)
	if replay.Text != first.Text {
		t.Fatalf("replayed callback changed the original code: %s", replay.Text)
	}
	assertIssuedDays(t, h, firstDay.ID)
	secondDay, err := h.store.CurrentDay(context.Background(), h.run.CampaignID, h.clock.Now())
	must(t, err)
	postSourceCallback(t, h, handler, h.clock.Now(), "original-button", "promo")
	fresh := executeSourceEvent(t, h)
	if fresh.Text == first.Text || !strings.Contains(fresh.Text, "Промокод: DEMO-") {
		t.Fatalf("fresh day callback did not issue a new code: %s", fresh.Text)
	}
	assertIssuedDays(t, h, firstDay.ID, secondDay.ID)
	assertSourceCallbackReplay(t, h, handler, source, "original-button", "promo", original.EventID)
	postSourceCallback(t, h, handler, source, "another-click-with-old-time", "promo")
	if old := executeSourceEvent(t, h); old.Text != first.Text {
		t.Fatalf("old callback returned the fresh day's code: %s", old.Text)
	}
	assertIssuedDays(t, h, firstDay.ID, secondDay.ID)
}

func TestFirstQueuedPromoAfterMidnightCannotClaimNextDayCode(t *testing.T) {
	h := setup(t, "standard")
	handler := sourceCallbackHandler(h)
	source := h.clock.Now()
	day, err := h.store.CurrentDay(context.Background(), h.run.CampaignID, source)
	must(t, err)
	source = day.EndsAt.Add(-time.Millisecond)
	h.clock.Set(source)
	original := postSourceCallback(t, h, handler, source, "original-button", "promo")
	h.clock.Set(day.EndsAt)
	late := executeSourceEvent(t, h)
	if strings.Contains(late.Text, "Промокод:") {
		t.Fatalf("late original event obtained a next-day code: %s", late.Text)
	}
	assertIssuedDays(t, h)
	currentDay, err := h.store.CurrentDay(context.Background(), h.run.CampaignID, h.clock.Now())
	must(t, err)
	postSourceCallback(t, h, handler, h.clock.Now(), "original-button", "promo")
	if fresh := executeSourceEvent(t, h); !strings.Contains(fresh.Text, "Промокод: DEMO-") {
		t.Fatalf("fresh current-day event failed: %s", fresh.Text)
	}
	assertIssuedDays(t, h, currentDay.ID)
	assertSourceCallbackReplay(t, h, handler, source, "original-button", "promo", original.EventID)
	postSourceCallback(t, h, handler, source, "new-click-with-old-time", "promo")
	if old := executeSourceEvent(t, h); strings.Contains(old.Text, "Промокод:") {
		t.Fatalf("old unfulfilled request reused a fresh day's assignment: %s", old.Text)
	}
	assertIssuedDays(t, h, currentDay.ID)
}

type subscriptionRecorder struct{ channels []int64 }

func (r *subscriptionRecorder) Subscription(_ context.Context, channelID, _ int64) (app.Subscription, error) {
	r.channels = append(r.channels, channelID)
	return app.SubscriptionYes, nil
}
func (r *subscriptionRecorder) Send(ctx context.Context, _ domain.Message) error { return ctx.Err() }

func TestQueuedSubscriptionCheckCannotSwitchPartnersAfterMidnight(t *testing.T) {
	h := setup(t, "standard")
	recorder := &subscriptionRecorder{}
	h.service = app.New(h.store, recorder, h.clock, firstCandidate{})
	handler := sourceCallbackHandler(h)
	source := h.clock.Now()
	original, err := h.store.CurrentDay(context.Background(), h.run.CampaignID, source)
	must(t, err)
	source = original.EndsAt.Add(-time.Millisecond)
	h.clock.Set(source)
	originalCallback := postSourceCallback(t, h, handler, source, "original-button", "check")
	h.clock.Set(original.EndsAt)
	late := executeSourceEvent(t, h)
	if strings.Contains(late.Text, "Заявка зарегистрирована") || len(recorder.channels) != 0 {
		t.Fatalf("late check contacted a different partner: channels=%v reply=%s", recorder.channels, late.Text)
	}
	active, err := h.store.Activities(context.Background(), h.run.CampaignID, h.run.UserIDs[0])
	must(t, err)
	if len(active) != 0 {
		t.Fatal("late check granted a chance")
	}
	current, err := h.store.CurrentDay(context.Background(), h.run.CampaignID, h.clock.Now())
	must(t, err)
	postSourceCallback(t, h, handler, h.clock.Now(), "original-button", "check")
	fresh := executeSourceEvent(t, h)
	if !strings.Contains(fresh.Text, "Заявка зарегистрирована") || len(recorder.channels) != 1 || recorder.channels[0] != current.ChannelID {
		t.Fatalf("new callback checked wrong partner: channels=%v reply=%s", recorder.channels, fresh.Text)
	}
	assertSourceCallbackReplay(t, h, handler, source, "original-button", "check", originalCallback.EventID)
	for _, action := range []string{"check", "participate"} {
		postSourceCallback(t, h, handler, source, "new-old-"+action, action)
		old := executeSourceEvent(t, h)
		if strings.Contains(old.Text, "Заявка зарегистрирована") || len(recorder.channels) != 1 {
			t.Fatalf("old %s switched to the current partner: channels=%v reply=%s", action, recorder.channels, old.Text)
		}
	}
	must(t, h.store.Within(context.Background(), func(tx app.Tx) error {
		oldEntry, err := tx.HasEntry(context.Background(), original.DrawID, h.run.UserIDs[0])
		if err != nil {
			return err
		}
		newEntry, err := tx.HasEntry(context.Background(), current.DrawID, h.run.UserIDs[0])
		if err != nil {
			return err
		}
		if oldEntry || !newEntry {
			t.Fatalf("check registered in wrong day: old=%v new=%v", oldEntry, newEntry)
		}
		return nil
	}))
	active, err = h.store.Activities(context.Background(), h.run.CampaignID, h.run.UserIDs[0])
	must(t, err)
	if len(active) != 1 || active[0].DayID != current.ID {
		t.Fatalf("wrong active day: %+v", active)
	}
}

func TestOldSourceTimeDoesNotDisableInformationOrPrizeClaims(t *testing.T) {
	h := setup(t, "drawable")
	now := h.clock.Now()
	past := now.Add(-24 * time.Hour)
	for _, action := range []string{"start", "help", "rules", "chances"} {
		cmd := h.command(action)
		cmd.OccurredAt = past
		if reply := h.execute(t, cmd); reply.Text == "" {
			t.Fatalf("old %s was not handled", action)
		}
	}
	_, err := h.service.RunDraw(context.Background(), h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	_, err = h.service.ApproveDraw(context.Background(), h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	offer := h.offers(t, h.run.DrawID)[0]
	cmd := h.command("claim_begin")
	cmd.UserID = offer.UserID
	cmd.OfferID = offer.ID
	cmd.OccurredAt = past
	if reply := h.execute(t, cmd); !strings.Contains(reply.Text, "имя и фамилию") {
		t.Fatalf("old source time blocked a current prize claim: %s", reply.Text)
	}
}
