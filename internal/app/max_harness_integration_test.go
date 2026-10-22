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
	query := "UPDATE " + pgx.Identifier{h.name, "draws"}.Sanitize() + `
		SET winner_count=2, reserve_count=1, claim_ttl_ns=$2
		WHERE id=$1`
	_, err := h.observer.Exec(context.Background(), query, h.run.DrawID, int64(time.Hour))
	must(t, err)
	provider := &maxWireProvider{membership: map[int64]string{}, checks: map[int64][]string{}}
	maxServer := httptest.NewServer(provider)
	t.Cleanup(maxServer.Close)
	messenger, err := maxapi.New(maxapi.Config{
		Token:     "local-e2e-token",
		BaseURL:   maxServer.URL,
		BannerURL: "https://example.org/e2e-banner.png",
	})
	must(t, err)
	h.service = app.New(h.store, messenger, h.clock, firstCandidate{})
	server := httptest.NewServer(httpapi.New(h.service, h.store, nil, messenger, httpapi.Config{
		Mode:          "production",
		CampaignID:    h.run.CampaignID,
		AdminToken:    "local-e2e-admin",
		WebhookSecret: "local-e2e-webhook",
	}))
	t.Cleanup(server.Close)
	workerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.service.Work(workerCtx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return &maxE2E{
		t:        t,
		h:        h,
		provider: provider,
		server:   server,
		client:   &http.Client{Timeout: 3 * time.Second},
	}
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
		query := "SELECT (SELECT count(*) FROM " + pgx.Identifier{e.h.name, "inbox"}.Sanitize() +
			" WHERE state<>'done')+(SELECT count(*) FROM " + pgx.Identifier{e.h.name, "outbox"}.Sanitize() +
			" WHERE state<>'delivered')"
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
		"update_type": "message_callback",
		"timestamp":   e.h.clock.Now().UnixMilli(),
		"message": map[string]any{
			"recipient": map[string]string{"chat_type": "dialog"},
		},
		"callback": map[string]any{
			"callback_id": id,
			"payload":     payload,
			"user":        map[string]int64{"user_id": user},
		},
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
		"update_type": "message_created",
		"timestamp":   e.h.clock.Now().UnixMilli(),
		"message": map[string]any{
			"sender":    map[string]int64{"user_id": user},
			"recipient": map[string]string{"chat_type": "dialog"},
			"body":      map[string]string{"mid": mid, "text": text},
		},
	}, &ack)
	return ack
}

func (e *maxE2E) offers() []generated.Offer {
	e.t.Helper()
	var list generated.OfferList
	e.request(http.MethodGet, "/api/admin/campaigns/"+e.h.run.CampaignID+"/draws/"+e.h.run.DrawID+"/offers", nil, &list)
	return list.Offers
}
