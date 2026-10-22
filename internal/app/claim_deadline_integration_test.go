//go:build integration

package app_test

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/app"
	"promo/internal/domain"
	"promo/internal/platform/maxapi"
)

func claimConversation(t *testing.T) (*harness, domain.Offer) {
	t.Helper()
	h := setup(t, "drawable")
	ctx := context.Background()
	_, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	_, err = h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	offer := h.offers(t, h.run.DrawID)[0]
	command := h.command("claim_begin")
	command.UserID, command.OfferID = offer.UserID, offer.ID
	h.execute(t, command)
	offer, err = h.store.Offer(ctx, offer.ID)
	must(t, err)
	return h, offer
}

func pendingClaim(t *testing.T, h *harness, userID int64) (string, string) {
	t.Helper()
	var offerID, name string
	must(t, h.store.Within(context.Background(), func(tx app.Tx) error {
		var err error
		offerID, name, err = tx.PendingClaim(context.Background(), userID)
		return err
	}))
	return offerID, name
}

func TestClaimNameRespectsDeadlineAndOfferState(t *testing.T) {
	for _, test := range []struct {
		name   string
		status string
		offset time.Duration
		valid  bool
	}{
		{"before_deadline", "notified", -time.Nanosecond, true},
		{"at_deadline", "notified", 0, false},
		{"after_deadline", "notified", time.Hour, false},
		{"expired", "expired", -time.Nanosecond, false},
		{"claimed", "claimed", -time.Nanosecond, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, offer := claimConversation(t)
			ctx := context.Background()
			h.clock.Set(offer.Deadline.Add(test.offset))
			if test.status != "notified" {
				offer.Status = test.status
				if test.status == "claimed" {
					now := h.clock.Now()
					offer.Name, offer.ClaimedAt, offer.ConsentAt = "Пётр Иванов", &now, &now
				}
				must(t, h.store.Within(ctx, func(tx app.Tx) error { return tx.SaveOffer(ctx, offer) }))
			}
			command := h.command("claim_name")
			command.UserID, command.Name = offer.UserID, "Иван Петров"
			reply := h.execute(t, command)
			pendingID, name := pendingClaim(t, h, offer.UserID)
			consent := len(reply.Buttons) == 1 && reply.Buttons[0].Payload == "claim:"+offer.ID
			if test.valid {
				if !consent || pendingID != offer.ID || name != command.Name {
					t.Fatalf("available offer lost its consent step: reply=%+v pending=%q name=%q", reply, pendingID, name)
				}
				return
			}
			if consent || pendingID != "" || name != "" {
				t.Fatalf("unavailable offer retained a claim conversation: reply=%+v pending=%q name=%q", reply, pendingID, name)
			}
			want := "Срок получения приза истёк."
			if test.status == "claimed" {
				want = "Получение приза уже подтверждено ✅"
			}
			if reply.Text != want {
				t.Fatalf("wrong unavailable-offer response: %q", reply.Text)
			}
			saved, err := h.store.Offer(ctx, offer.ID)
			must(t, err)
			if saved.Name != offer.Name || saved.Status != offer.Status {
				t.Fatalf("name input changed the prize: before=%+v after=%+v", offer, saved)
			}
		})
	}
}

func TestClaimNameChecksCurrentTimeAfterWaitingForDraw(t *testing.T) {
	h, offer := claimConversation(t)
	ctx := context.Background()
	h.clock.Set(offer.Deadline.Add(-time.Second))
	release := h.holdLock(t, func(tx app.Tx) error { _, err := tx.Draw(ctx, offer.DrawID, true); return err })
	command := h.command("claim_name")
	command.UserID, command.Name = offer.UserID, "Иван Петров"
	type result struct {
		reply domain.Message
		err   error
	}
	done := make(chan result, 1)
	go func() { reply, err := h.service.Execute(ctx, command); done <- result{reply, err} }()
	h.waitForLock(t, "FROM draws")
	h.clock.Set(*offer.Deadline)
	release()
	select {
	case result := <-done:
		must(t, result.err)
		if result.reply.Text != "Срок получения приза истёк." {
			t.Fatalf("waiting for the draw preserved a stale deadline: %+v", result.reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("claim name did not resume after releasing the draw")
	}
	if pendingID, name := pendingClaim(t, h, offer.UserID); pendingID != "" || name != "" {
		t.Fatalf("expired conversation survived: offer=%q name=%q", pendingID, name)
	}
}

func TestClaimNameAndExpirationLeaveOneReserveAndNoPendingName(t *testing.T) {
	h, offer := claimConversation(t)
	ctx := context.Background()
	h.clock.Set(*offer.Deadline)
	concurrently(t, 24, func(i int) error {
		if i%2 == 0 {
			return h.service.ExpireOffer(ctx, offer.ID)
		}
		command := h.command("claim_name")
		command.UserID, command.Name = offer.UserID, "Иван Петров"
		_, err := h.service.Execute(ctx, command)
		return err
	})
	if pendingID, name := pendingClaim(t, h, offer.UserID); pendingID != "" || name != "" {
		t.Fatalf("expired name was retained: offer=%q name=%q", pendingID, name)
	}
	offers := h.offers(t, offer.DrawID)
	if len(offers) != 2 {
		t.Fatalf("expiration must create exactly one reserve offer: %+v", offers)
	}
	for _, current := range offers {
		if current.ID == offer.ID && (current.Status != "expired" || current.Name != "") {
			t.Fatalf("expired offer accepted personal data: %+v", current)
		}
	}
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	var reserveNotifications int
	for _, message := range messages {
		for _, button := range message.Buttons {
			if button.Payload == "claim:"+offer.ID {
				t.Fatalf("expired prize offered consent: %+v", message)
			}
		}
		if message.OfferID != "" && message.OfferID != offer.ID {
			reserveNotifications++
		}
	}
	if reserveNotifications != 1 {
		t.Fatalf("reserve notification count=%d", reserveNotifications)
	}
}

func TestClaimNameRejectsAnotherUsersPrize(t *testing.T) {
	h, offer := claimConversation(t)
	ctx := context.Background()
	otherUser := h.run.UserIDs[1]
	must(t, h.store.Within(ctx, func(tx app.Tx) error {
		return tx.SetPendingClaim(ctx, otherUser, offer.ID, "")
	}))
	command := h.command("claim_name")
	command.UserID, command.Name = otherUser, "Иван Петров"
	reply := h.execute(t, command)
	if reply.Text != "Этот приз предназначен другому участнику." {
		t.Fatalf("another participant's prize accepted a name: %+v", reply)
	}
	if pendingID, name := pendingClaim(t, h, otherUser); pendingID != "" || name != "" {
		t.Fatalf("invalid conversation retained data: offer=%q name=%q", pendingID, name)
	}
	if pendingID, _ := pendingClaim(t, h, offer.UserID); pendingID != offer.ID {
		t.Fatalf("the owner's conversation was removed: offer=%q", pendingID)
	}
}

func TestExpirationClearsOnlyItsPendingConversation(t *testing.T) {
	h, offer := claimConversation(t)
	h.clock.Set(*offer.Deadline)
	must(t, h.service.ExpireOffer(context.Background(), offer.ID))
	if pendingID, name := pendingClaim(t, h, offer.UserID); pendingID != "" || name != "" {
		t.Fatalf("expiration retained the name entry state: offer=%q name=%q", pendingID, name)
	}
}

type delayedExpirationRepository struct{ app.Repository }

func (delayedExpirationRepository) DueOffers(context.Context, time.Time) ([]domain.Offer, error) {
	return nil, nil
}

func (delayedExpirationRepository) DueDraws(context.Context, time.Time) ([]domain.Draw, error) {
	return nil, nil
}

type claimNotificationRecorder struct{ calls atomic.Int32 }

func (*claimNotificationRecorder) Subscription(context.Context, int64, int64) (app.Subscription, error) {
	return app.SubscriptionYes, nil
}

func (m *claimNotificationRecorder) Send(ctx context.Context, _ domain.Message) error {
	m.calls.Add(1)
	return ctx.Err()
}

func TestRecoveredPrizeNotificationIsSkippedAtDeadline(t *testing.T) {
	for _, offset := range []time.Duration{0, time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			h := setup(t, "drawable")
			ctx := context.Background()
			_, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
			must(t, err)
			_, err = h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
			must(t, err)
			offer := h.offers(t, h.run.DrawID)[0]
			job, err := h.store.TakeMessage(ctx, h.clock.Now())
			must(t, err)
			if job == nil || job.OfferID != offer.ID || job.Attempts != 1 {
				t.Fatalf("prize notification was not leased: %+v", job)
			}
			must(t, h.service.MarkDelivered(ctx, offer.ID))
			notified, err := h.store.Offer(ctx, offer.ID)
			must(t, err)
			h.clock.Set(notified.Deadline.Add(offset))
			messenger := &claimNotificationRecorder{}
			h.service = app.New(delayedExpirationRepository{h.store}, messenger, h.clock, firstCandidate{}, app.WithOutboxWorkers(1))
			workerCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { h.service.Work(workerCtx, slog.New(slog.NewTextHandler(io.Discard, nil))); close(done) }()
			defer func() { cancel(); <-done }()
			awaitWorkerProgress(t, "the recovered prize notification", func() bool {
				messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
				must(t, err)
				return len(messages) == 1 && messages[0].State == "delivered"
			})
			cancel()
			<-done
			if messenger.calls.Load() != 0 {
				t.Fatal("recovered notification announced an unavailable prize")
			}
			after, err := h.store.Offer(ctx, offer.ID)
			must(t, err)
			if after.Status != "notified" || !after.Deadline.Equal(*notified.Deadline) || !after.SentAt.Equal(*notified.SentAt) {
				t.Fatalf("skipping a stale notification changed the claim period: %+v", after)
			}
			messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
			must(t, err)
			if messages[0].Attempts != 2 {
				t.Fatalf("crashed job was not recovered: %+v", messages[0])
			}
			if next, err := h.store.TakeMessage(ctx, h.clock.Now().Add(time.Minute)); err != nil || next != nil {
				t.Fatalf("obsolete notification was not acknowledged: message=%+v error=%v", next, err)
			}
			must(t, h.service.ExpireOffer(ctx, offer.ID))
			offers := h.offers(t, offer.DrawID)
			if len(offers) != 2 {
				t.Fatalf("skipping a stale notification prevented reserve handover: %+v", offers)
			}
		})
	}
}

func TestPrizeNotificationExpiresWhileWaitingForMAX(t *testing.T) {
	h := setup(t, "drawable")
	ctx := context.Background()
	_, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	_, err = h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	offer := h.offers(t, h.run.DrawID)[0]
	job, err := h.store.TakeMessage(ctx, h.clock.Now())
	must(t, err)
	if job == nil || job.OfferID != offer.ID {
		t.Fatalf("prize notification was not leased: %+v", job)
	}
	must(t, h.service.MarkDelivered(ctx, offer.ID))
	h.clock.Set(h.clock.Now().Add(31 * time.Second))
	notified, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	deadline := h.clock.Now().Add(80 * time.Millisecond)
	notified.Deadline = &deadline
	must(t, h.store.Within(ctx, func(tx app.Tx) error {
		if err := tx.SaveOffer(ctx, notified); err != nil {
			return err
		}
		return tx.SetPendingClaim(ctx, offer.UserID, offer.ID, "")
	}))
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/messages" {
			t.Errorf("unexpected MAX request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"message":{"body":{"mid":"delivered"}}}`)
	}))
	defer provider.Close()
	sender, err := maxapi.New(maxapi.Config{Token: "local-test-token", BaseURL: provider.URL})
	must(t, err)
	must(t, sender.Send(ctx, domain.Message{UserID: offer.UserID, Text: "Меню"}))
	h.service = app.New(delayedExpirationRepository{h.store}, sender, h.clock, firstCandidate{}, app.WithOutboxWorkers(1))
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { h.service.Work(workerCtx, slog.New(slog.NewTextHandler(io.Discard, nil))); close(done) }()
	defer func() { cancel(); <-done }()
	awaitWorkerProgress(t, "the notification waiting for MAX", func() bool {
		messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
		must(t, err)
		return len(messages) == 1 && messages[0].State == "delivered"
	})
	cancel()
	<-done
	if calls.Load() != 1 {
		t.Fatalf("an expired prize reached MAX after waiting for its dialog limit: requests=%d", calls.Load())
	}
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if messages[0].Attempts != 2 {
		t.Fatalf("the crashed notification was not recovered: %+v", messages[0])
	}
	after, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if after.Status != "notified" || !after.Deadline.Equal(deadline) || !after.SentAt.Equal(*notified.SentAt) {
		t.Fatalf("cancelling a stale send changed the claim period: %+v", after)
	}
	if pendingID, _ := pendingClaim(t, h, offer.UserID); pendingID != offer.ID {
		t.Fatalf("notification cancellation changed an unfinished conversation: %q", pendingID)
	}
	h.clock.Set(deadline)
	if next, err := h.store.TakeMessage(ctx, h.clock.Now().Add(time.Minute)); err != nil || next != nil {
		t.Fatalf("the obsolete notification was not acknowledged: message=%+v error=%v", next, err)
	}
	must(t, h.service.ExpireOffer(ctx, offer.ID))
	if pendingID, name := pendingClaim(t, h, offer.UserID); pendingID != "" || name != "" {
		t.Fatalf("expiration retained an obsolete conversation: offer=%q name=%q", pendingID, name)
	}
	offers := h.offers(t, offer.DrawID)
	if len(offers) != 2 {
		t.Fatalf("expiry did not create one reserve offer: %+v", offers)
	}
	reserve, err := h.store.TakeMessage(ctx, h.clock.Now())
	must(t, err)
	if reserve == nil || reserve.OfferID == offer.ID || reserve.UserID != h.run.UserIDs[1] || reserve.Attempts != 1 {
		t.Fatalf("wrong reserve notification after cancelled delivery: %+v", reserve)
	}
	var oldState string
	must(t, h.observer.QueryRow(ctx, "SELECT state FROM "+pgx.Identifier{h.name, "outbox"}.Sanitize()+" WHERE id=$1", job.ID).Scan(&oldState))
	if oldState != "delivered" {
		t.Fatalf("reserve handover reopened an obsolete job: %s", oldState)
	}
}

func TestExpirationKeepsAnotherPrizeConversation(t *testing.T) {
	h, offer := claimConversation(t)
	ctx := context.Background()
	otherID := setAnotherPrizeConversation(t, h, offer.UserID)
	h.clock.Set(*offer.Deadline)
	must(t, h.service.ExpireOffer(ctx, offer.ID))
	if pendingID, name := pendingClaim(t, h, offer.UserID); pendingID != otherID || name != "Иван Петров" {
		t.Fatalf("expiration cleared a different prize conversation: offer=%q name=%q", pendingID, name)
	}
	command := h.command("claim_begin")
	command.UserID, command.OfferID = offer.UserID, offer.ID
	if reply := h.execute(t, command); !strings.Contains(reply.Text, "истёк") {
		t.Fatalf("expired prize was available: %+v", reply)
	}
	if pendingID, name := pendingClaim(t, h, offer.UserID); pendingID != otherID || name != "Иван Петров" {
		t.Fatalf("replayed expired button cleared a different conversation: offer=%q name=%q", pendingID, name)
	}
}

func setAnotherPrizeConversation(t *testing.T, h *harness, userID int64) string {
	t.Helper()
	ctx := context.Background()
	otherID := "offer_" + rand.Text()
	must(t, h.store.Within(ctx, func(tx app.Tx) error {
		if _, err := tx.UserForUpdate(ctx, userID, h.clock.Now()); err != nil {
			return err
		}
		slotID := "slot_" + rand.Text()
		if err := tx.CreateSlot(ctx, slotID, h.run.FinalDrawID, 1); err != nil {
			return err
		}
		if err := tx.SaveOffer(ctx, domain.Offer{ID: otherID, SlotID: slotID, DrawID: h.run.FinalDrawID, UserID: userID, Status: "pending"}); err != nil {
			return err
		}
		return tx.SetPendingClaim(ctx, userID, otherID, "Иван Петров")
	}))
	return otherID
}

func TestClaimConfirmationKeepsAnotherPrizeConversation(t *testing.T) {
	h, offer := claimConversation(t)
	otherID := setAnotherPrizeConversation(t, h, offer.UserID)
	claim := h.command("claim")
	claim.UserID, claim.OfferID, claim.Name, claim.Consent = offer.UserID, offer.ID, "Пётр Сидоров", true
	h.execute(t, claim)
	confirmed, err := h.store.Offer(context.Background(), offer.ID)
	must(t, err)
	if confirmed.Status != "claimed" || confirmed.Name != claim.Name {
		t.Fatalf("claim was not confirmed: %+v", confirmed)
	}
	for _, action := range []string{"claim", "claim_begin"} {
		command := h.command(action)
		command.UserID, command.OfferID = offer.UserID, offer.ID
		if reply := h.execute(t, command); reply.Text != "Получение приза уже подтверждено ✅" {
			t.Fatalf("replayed %s lost the confirmation: %+v", action, reply)
		}
	}
	if pendingID, name := pendingClaim(t, h, offer.UserID); pendingID != otherID || name != "Иван Петров" {
		t.Fatalf("confirming one prize removed another conversation: offer=%q name=%q", pendingID, name)
	}
}

func TestClaimNameDoesNotOverwriteAChangedConversation(t *testing.T) {
	h, offer := claimConversation(t)
	ctx := context.Background()
	release := h.holdLock(t, func(tx app.Tx) error { _, err := tx.Draw(ctx, offer.DrawID, true); return err })
	command := h.command("claim_name")
	command.UserID, command.Name = offer.UserID, "Пётр Сидоров"
	done := make(chan error, 1)
	go func() { _, err := h.service.Execute(ctx, command); done <- err }()
	h.waitForLock(t, "FROM draws")
	otherID := setAnotherPrizeConversation(t, h, offer.UserID)
	release()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("claim name did not resume after the conversation changed")
	}
	if pendingID, name := pendingClaim(t, h, offer.UserID); pendingID != otherID || name != "Иван Петров" {
		t.Fatalf("stale name input overwrote another prize conversation: offer=%q name=%q", pendingID, name)
	}
}
