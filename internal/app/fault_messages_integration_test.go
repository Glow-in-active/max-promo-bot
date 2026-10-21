//go:build integration

package app_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
)

type faultMessageRepository struct {
	app.Repository
	err error
}

func (r faultMessageRepository) Within(ctx context.Context, fn func(app.Tx) error) error {
	return r.Repository.Within(ctx, func(tx app.Tx) error {
		return fn(faultMessageTransaction{Tx: tx, err: r.err})
	})
}

type faultMessageTransaction struct {
	app.Tx
	err error
}

func (tx faultMessageTransaction) RecordPromoRequest(context.Context, string, int64, time.Time) error {
	return tx.err
}

func TestApplicationFaultRepliesUseBotTextWithoutTechnicalDetails(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"localized", domain.UserFail(domain.NotActive, "Campaign is not active.", "Акция сейчас не активна."), "Акция сейчас не активна."},
		{"technical-only", domain.Fail(domain.NotFound, "Internal promo pool not found."), "Не удалось выполнить действие. Попробуйте ещё раз чуть позже."},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := setup(t, "standard")
			repo := faultMessageRepository{Repository: h.store, err: fmt.Errorf("record promo request: %w", test.err)}
			service := app.New(repo, h.messenger, h.clock, firstCandidate{})
			reply, err := service.Execute(context.Background(), h.command("promo"))
			must(t, err)
			if reply.Text != test.want || len(reply.Buttons) == 0 {
				t.Fatalf("bot reply exposed technical details or lost navigation: %+v", reply)
			}
			messages, err := h.store.Messages(context.Background(), h.run.CampaignID, "")
			must(t, err)
			if len(messages) != 1 || messages[0].Text != test.want {
				t.Fatalf("durable bot reply differs from the localized result: %+v", messages)
			}
		})
	}
}
