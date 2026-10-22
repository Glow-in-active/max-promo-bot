//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"promo/internal/app"
	"promo/internal/domain"
	"promo/internal/platform/maxapi"
)

func TestLegacyOversizedPrizeFailsBeforeMAX(t *testing.T) {
	h := setup(t, "drawable")
	ctx := context.Background()
	query := "UPDATE " + pgx.Identifier{h.name, "draws"}.Sanitize() + " SET prize=$2 WHERE id=$1"
	_, err := h.observer.Exec(ctx, query, h.run.DrawID, strings.Repeat("П", 4000))
	must(t, err)
	assertPrizeRejectedBeforeMAX(t, h, nil)
}

func TestLegacyInvalidButtonsFailBeforeMAX(t *testing.T) {
	for _, test := range []struct {
		name   string
		button domain.Button
	}{
		{name: "empty text", button: domain.Button{Payload: "claim"}},
		{name: "long text", button: domain.Button{Text: strings.Repeat("П", 129), Payload: "claim"}},
		{name: "long payload", button: domain.Button{Text: "Claim", Payload: strings.Repeat("🎁", 1025)}},
		{name: "long URL", button: domain.Button{Text: "Claim", URL: "https://max.ru/" + strings.Repeat("П", 2048)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertPrizeRejectedBeforeMAX(t, setup(t, "drawable"), []domain.Button{test.button})
		})
	}
}

func assertPrizeRejectedBeforeMAX(t *testing.T, h *harness, legacyButtons []domain.Button) {
	t.Helper()
	ctx := context.Background()
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"message":{"body":{"mid":"sent"}}}`))
	}))
	defer provider.Close()
	sender, err := maxapi.New(maxapi.Config{Token: "local-test-token", BaseURL: provider.URL})
	must(t, err)
	h.service = app.New(h.store, sender, h.clock, firstCandidate{})
	_, err = h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	_, err = h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	offer := h.offers(t, h.run.DrawID)[0]
	if legacyButtons != nil {
		data, err := json.Marshal(legacyButtons)
		must(t, err)
		query := "UPDATE " + pgx.Identifier{h.name, "outbox"}.Sanitize() + " SET buttons=$2 WHERE offer_id=$1"
		_, err = h.observer.Exec(ctx, query, offer.ID, data)
		must(t, err)
	}
	stop := startTestWorkers(t, h)
	awaitWorkerProgress(t, "invalid message rejection", func() bool {
		messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
		must(t, err)
		return len(messages) == 1 && messages[0].State == "failed"
	})
	stop()
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if len(messages) != 1 || messages[0].State != "failed" || messages[0].Attempts != 1 || requests.Load() != 0 {
		t.Fatalf("invalid notification reached MAX or kept retrying: messages=%+v requests=%d", messages, requests.Load())
	}
	offer, err = h.store.Offer(ctx, offer.ID)
	must(t, err)
	if offer.Status != "pending" || offer.SentAt != nil || offer.Deadline != nil {
		t.Fatalf("rejected notification started the claim period: %+v", offer)
	}
}

func TestMAXLongPartnerKeepsShortSubscriptionButton(t *testing.T) {
	e := newMAXE2E(t)
	partner := strings.Repeat("П", 1000)
	query := "UPDATE " + pgx.Identifier{e.h.name, "days"}.Sanitize() + " SET partner=$2 WHERE id=$1"
	_, err := e.h.observer.Exec(context.Background(), query, e.h.run.DayID, partner)
	must(t, err)
	day, err := e.h.store.CurrentDay(context.Background(), e.h.run.CampaignID, e.h.clock.Now())
	must(t, err)
	user := e.h.run.UserIDs[0]
	e.callback(user, "participate")
	message := e.message(0, user, func(message maxWireMessage) bool {
		return strings.Contains(message.Text, partner)
	})
	links := 0
	for _, attachment := range message.Attachments {
		for _, row := range attachment.Payload.Buttons {
			for _, button := range row {
				if button.Type == "link" {
					links++
					if button.Text != "Подписаться на канал" || button.URL != day.PartnerURL {
						t.Fatalf("unexpected subscription button: %+v", button)
					}
				}
			}
		}
	}
	if links != 1 || message.callback("check") != "check" {
		t.Fatalf("subscription controls missing: links=%d", links)
	}
}
