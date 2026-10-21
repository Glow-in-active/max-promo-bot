//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/app"
	"promo/internal/platform/maxapi"
	"promo/internal/transport/httpapi"
	"promo/internal/transport/httpapi/generated"
)

type maxWireMessage struct {
	UserID      int64
	Text        string `json:"text"`
	Attachments []struct {
		Type    string `json:"type"`
		Payload struct {
			URL     string `json:"url"`
			Buttons [][]struct {
				Type, Text, Payload, URL string
			} `json:"buttons"`
		} `json:"payload"`
	} `json:"attachments"`
}

func (m maxWireMessage) callback(prefix string) string {
	for _, attachment := range m.Attachments {
		if attachment.Type != "inline_keyboard" {
			continue
		}
		for _, row := range attachment.Payload.Buttons {
			for _, button := range row {
				if button.Type == "callback" && strings.HasPrefix(button.Payload, prefix) {
					return button.Payload
				}
			}
		}
	}
	return ""
}

type maxWireProvider struct {
	mu         sync.Mutex
	membership map[int64]string
	checks     map[int64][]string
	messages   []maxWireMessage
	answers    []string
}

func (p *maxWireProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Header.Get("Authorization") != "local-e2e-token" {
		http.Error(w, "wrong provider authorization", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/members"):
		user, _ := strconv.ParseInt(r.URL.Query().Get("user_ids"), 10, 64)
		p.checks[user] = append(p.checks[user], r.URL.Path)
		switch p.membership[user] {
		case "yes":
			_ = json.NewEncoder(w).Encode(map[string]any{"members": []map[string]int64{{"user_id": user}}})
		case "error":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"code":"unavailable"}`)
		default:
			_, _ = io.WriteString(w, `{"members":[]}`)
		}
	case r.Method == http.MethodPost && r.URL.Path == "/messages":
		var message maxWireMessage
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			http.Error(w, "invalid message", http.StatusBadRequest)
			return
		}
		message.UserID, _ = strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
		p.messages = append(p.messages, message)
		_, _ = io.WriteString(w, `{"message":{"body":{"mid":"local-delivery"}}}`)
	case r.Method == http.MethodPost && r.URL.Path == "/answers":
		p.answers = append(p.answers, r.URL.Query().Get("callback_id"))
		_, _ = io.WriteString(w, `{"success":true}`)
	default:
		http.NotFound(w, r)
	}
}

func (p *maxWireProvider) setMembership(user int64, status string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.membership[user] = status
}

func (p *maxWireProvider) sent() []maxWireMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]maxWireMessage(nil), p.messages...)
}

type maxE2E struct {
	t        *testing.T
	h        *harness
	provider *maxWireProvider
	server   *httptest.Server
	client   *http.Client
	sequence int
}

func newMAXE2E(t *testing.T) *maxE2E {
	t.Helper()
	h := setupAt(t, "standard", time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond))
	_, err := h.observer.Exec(context.Background(), "UPDATE "+pgx.Identifier{h.name, "draws"}.Sanitize()+" SET winner_count=2,reserve_count=1,claim_ttl_ns=$2 WHERE id=$1", h.run.DrawID, int64(time.Hour))
	must(t, err)
	provider := &maxWireProvider{membership: map[int64]string{}, checks: map[int64][]string{}}
	maxServer := httptest.NewServer(provider)
	t.Cleanup(maxServer.Close)
	messenger, err := maxapi.New(maxapi.Config{Token: "local-e2e-token", BaseURL: maxServer.URL, BannerURL: "https://example.org/e2e-banner.png"})
	must(t, err)
	h.service = app.New(h.store, messenger, h.clock, firstCandidate{})
	server := httptest.NewServer(httpapi.New(h.service, h.store, nil, messenger, httpapi.Config{
		Mode: "production", CampaignID: h.run.CampaignID, AdminToken: "local-e2e-admin", WebhookSecret: "local-e2e-webhook",
	}))
	t.Cleanup(server.Close)
	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.service.Work(workerCtx, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	t.Cleanup(func() { cancel(); <-done })
	return &maxE2E{t: t, h: h, provider: provider, server: server, client: &http.Client{Timeout: 3 * time.Second}}
}

func (e *maxE2E) request(method, path string, body any, out any) {
	e.t.Helper()
	var input bytes.Buffer
	if body != nil {
		must(e.t, json.NewEncoder(&input).Encode(body))
	}
	req, err := http.NewRequest(method, e.server.URL+path, &input)
	must(e.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer local-e2e-admin")
	req.Header.Set("X-Max-Bot-Api-Secret", "local-e2e-webhook")
	response, err := e.client.Do(req)
	must(e.t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	must(e.t, err)
	if response.StatusCode != http.StatusOK {
		e.t.Fatalf("%s %s returned %d: %s", method, path, response.StatusCode, data)
	}
	if out != nil {
		must(e.t, json.Unmarshal(data, out))
	}
}

func (e *maxE2E) await(description string, ready func() bool) {
	e.t.Helper()
	timeout := time.NewTimer(8 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-timeout.C:
			e.t.Fatal("did not observe " + description)
		case <-tick.C:
		}
	}
}

func (e *maxE2E) idle() {
	e.t.Helper()
	e.await("durable inbox and outbox completion", func() bool {
		var pending int
		query := "SELECT (SELECT count(*) FROM " + pgx.Identifier{e.h.name, "inbox"}.Sanitize() + " WHERE state<>'done')+(SELECT count(*) FROM " + pgx.Identifier{e.h.name, "outbox"}.Sanitize() + " WHERE state<>'delivered')"
		must(e.t, e.h.observer.QueryRow(context.Background(), query).Scan(&pending))
		return pending == 0
	})
}

func (e *maxE2E) message(after int, user int64, predicate func(maxWireMessage) bool) maxWireMessage {
	e.t.Helper()
	var found maxWireMessage
	e.await("MAX message to "+strconv.FormatInt(user, 10), func() bool {
		for _, m := range e.provider.sent()[after:] {
			if m.UserID == user && predicate(m) {
				found = m
				return true
			}
		}
		return false
	})
	e.idle()
	return found
}

func (e *maxE2E) callback(user int64, payload string) generated.WebhookAck {
	e.t.Helper()
	e.sequence++
	id := fmt.Sprintf("original-callback-%d", e.sequence)
	body := map[string]any{
		"update_type": "message_callback", "timestamp": e.h.clock.Now().UnixMilli(),
		"message":  map[string]any{"recipient": map[string]string{"chat_type": "dialog"}},
		"callback": map[string]any{"callback_id": id, "payload": payload, "user": map[string]int64{"user_id": user}},
	}
	var ack generated.WebhookAck
	e.request(http.MethodPost, "/webhooks/max", body, &ack)
	if !ack.Accepted || ack.EventID == "" {
		e.t.Fatalf("callback not durably accepted: %+v", ack)
	}
	e.provider.mu.Lock()
	defer e.provider.mu.Unlock()
	if len(e.provider.answers) == 0 || e.provider.answers[len(e.provider.answers)-1] != id {
		e.t.Fatal("original callback ID was not acknowledged through MAX /answers")
	}
	return ack
}

func (e *maxE2E) text(user int64, mid, text string) generated.WebhookAck {
	e.t.Helper()
	var ack generated.WebhookAck
	e.request(http.MethodPost, "/webhooks/max", map[string]any{
		"update_type": "message_created", "timestamp": e.h.clock.Now().UnixMilli(),
		"message": map[string]any{"sender": map[string]int64{"user_id": user}, "recipient": map[string]string{"chat_type": "dialog"}, "body": map[string]string{"mid": mid, "text": text}},
	}, &ack)
	return ack
}

func (e *maxE2E) offers() []generated.Offer {
	e.t.Helper()
	var list generated.OfferList
	e.request(http.MethodGet, "/api/admin/campaigns/"+e.h.run.CampaignID+"/draws/"+e.h.run.DrawID+"/offers", nil, &list)
	return list.Offers
}

func TestMAXEndToEndCampaignLifecycle(t *testing.T) {
	e := newMAXE2E(t)
	h := e.h
	ctx := context.Background()
	day, err := h.store.CurrentDay(ctx, h.run.CampaignID, h.clock.Now())
	must(t, err)
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
	user := h.run.UserIDs[0]
	e.request(http.MethodPost, "/webhooks/max", map[string]any{"update_type": "bot_started", "timestamp": h.clock.Now().UnixMilli(), "chat_id": user, "user": map[string]int64{"user_id": user}}, nil)
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
	var lastRegistration generated.WebhookAck
	for _, participant := range h.run.UserIDs[:3] {
		e.provider.setMembership(participant, "yes")
		start = len(e.provider.sent())
		lastRegistration = e.callback(participant, "check")
		e.message(start, participant, func(m maxWireMessage) bool {
			return strings.Contains(m.Text, "Заявка зарегистрирована") && strings.Contains(m.Text, "Накоплено шансов: 1 из 10")
		})
	}
	start = len(e.provider.sent())
	beforeReplay, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	e.sequence--
	replayedUser := h.run.UserIDs[2]
	replayedCallback := e.callback(replayedUser, "check")
	e.idle()
	afterReplay, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if !replayedCallback.Duplicate || replayedCallback.EventID != lastRegistration.EventID || len(e.provider.sent()) != start || !reflect.DeepEqual(beforeReplay, afterReplay) {
		t.Fatalf("exact callback replay created inbox/outbox work: first=%+v repeated=%+v messages=%d/%d", lastRegistration, replayedCallback, len(beforeReplay), len(afterReplay))
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
	e.request(http.MethodPost, drawPath+"/approve", nil, nil)
	prizes := map[int64]maxWireMessage{}
	for _, winner := range drawn.Selection.Winners {
		prizes[winner] = e.message(0, winner, func(m maxWireMessage) bool { return m.callback("claim_begin:") != "" })
	}
	offers := e.offers()
	if len(offers) != 2 {
		t.Fatalf("approval created %d offers", len(offers))
	}
	var claimedID, expiredID string
	winner := drawn.Selection.Winners[0]
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
	start = len(e.provider.sent())
	e.callback(winner, prizes[winner].callback("claim_begin:"))
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
	start = len(e.provider.sent())
	e.callback(winner, consent.callback("claim:"))
	e.message(start, winner, func(m maxWireMessage) bool {
		return strings.Contains(m.Text, "Получение приза подтверждено")
	})
	var originalClaim generated.Offer
	var deadline time.Time
	for _, offer := range e.offers() {
		if offer.ID == claimedID {
			originalClaim = offer
			if offer.Status != "claimed" || offer.Name != "Иван Петров" || offer.ConsentAt.IsZero() || offer.ClaimedAt.IsZero() {
				t.Fatalf("name and consent were not persisted: %+v", offer)
			}
		}
		if offer.ID == expiredID {
			deadline = offer.Deadline
		}
	}
	start = len(e.provider.sent())
	e.callback(winner, consent.callback("claim:"))
	e.message(start, winner, func(m maxWireMessage) bool { return strings.Contains(m.Text, "уже подтверждено") })
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
	offers = e.offers()
	if len(offers) != 3 {
		t.Fatalf("expiry or approval replay duplicated replacement: %+v", offers)
	}
	for _, offer := range offers {
		switch offer.ID {
		case claimedID:
			if !reflect.DeepEqual(offer, originalClaim) {
				t.Fatalf("expiry changed the claimed winner: %+v", offer)
			}
		case expiredID:
			if offer.Status != "expired" {
				t.Fatalf("unclaimed winner did not expire at deadline: %+v", offer)
			}
		default:
			if offer.UserID != reserve || offer.Status != "notified" || reserveMessage.callback("claim_begin:") != "claim_begin:"+offer.ID || !offer.Deadline.Equal(deadline.Add(time.Hour)) {
				t.Fatalf("replacement did not receive its own full deadline: %+v", offer)
			}
		}
	}
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
