//go:build integration

package app_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/app"
	"promo/internal/domain"
)

type enqueueFailpointRepository struct {
	app.Repository
	failAt int
	calls  int
	err    error
}

func (r *enqueueFailpointRepository) Within(ctx context.Context, fn func(app.Tx) error) error {
	return r.Repository.Within(ctx, func(tx app.Tx) error {
		return fn(&enqueueFailpointTransaction{Tx: tx, repo: r})
	})
}

type enqueueFailpointTransaction struct {
	app.Tx
	repo *enqueueFailpointRepository
}

func (tx *enqueueFailpointTransaction) Enqueue(ctx context.Context, message domain.Message, key string, now time.Time) error {
	tx.repo.calls++
	if tx.repo.calls == tx.repo.failAt {
		return tx.repo.err
	}
	return tx.Tx.Enqueue(ctx, message, key, now)
}

func TestApplicationPromoFailureRollsBackBusinessAndInboxBeforeRetry(t *testing.T) {
	h := setup(t, "standard")
	ctx := context.Background()
	fault := errors.New("injected enqueue failure")
	repo := &enqueueFailpointRepository{Repository: h.store, failAt: 1, err: fault}
	service := app.New(repo, h.messenger, h.clock, firstCandidate{})
	command := h.command("promo")
	command.OccurredAt = h.clock.Now()
	inserted, err := h.store.PutInbox(ctx, command, "promo-rollback")
	must(t, err)
	if !inserted {
		t.Fatal("event was not inserted")
	}
	event, err := h.store.TakeInbox(ctx, time.Now().Add(time.Second))
	must(t, err)
	if event == nil || event.ID != command.ID || event.Attempts != 1 {
		t.Fatalf("first lease: %+v", event)
	}
	_, err = service.Execute(ctx, event.Command)
	if !errors.Is(err, fault) || repo.calls != 1 {
		t.Fatalf("fault was not reached: calls=%d error=%v", repo.calls, err)
	}
	stats, err := h.store.Stats(ctx, h.run.CampaignID)
	must(t, err)
	activities, err := h.store.Activities(ctx, h.run.CampaignID, command.UserID)
	must(t, err)
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if stats.PromoIssued != 0 || stats.PromoRequests != 0 || len(activities) != 0 || len(messages) != 0 {
		t.Fatalf("partial transaction survived: stats=%+v activities=%+v messages=%+v", stats, activities, messages)
	}
	must(t, h.store.Within(ctx, func(tx app.Tx) error {
		assignment, err := tx.Assignment(ctx, h.run.DayID, command.UserID)
		if err != nil {
			return err
		}
		done, err := tx.InboxDone(ctx, command.ID)
		if assignment != nil || done {
			t.Errorf("rolled-back assignment=%+v inbox_done=%t", assignment, done)
		}
		return err
	}))
	must(t, h.store.RetryInbox(ctx, event.ID, fault.Error(), h.clock.Now(), false))
	retry, err := h.store.TakeInbox(ctx, h.clock.Now())
	must(t, err)
	if retry == nil || retry.ID != event.ID || retry.Attempts != 2 || !reflect.DeepEqual(retry.Command, event.Command) {
		t.Fatalf("retry changed the durable event: first=%+v retry=%+v", event, retry)
	}
	_, err = service.Execute(ctx, retry.Command)
	must(t, err)
	_, err = service.Execute(ctx, retry.Command)
	must(t, err)
	stats, err = h.store.Stats(ctx, h.run.CampaignID)
	must(t, err)
	activities, err = h.store.Activities(ctx, h.run.CampaignID, command.UserID)
	must(t, err)
	messages, err = h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if stats.PromoIssued != 1 || stats.PromoRequests != 1 || len(activities) != 1 || len(messages) != 1 {
		t.Fatalf("retry/replay was not exactly one committed mutation: stats=%+v activities=%+v messages=%+v", stats, activities, messages)
	}
	must(t, h.store.Within(ctx, func(tx app.Tx) error {
		done, err := tx.InboxDone(ctx, command.ID)
		if !done {
			t.Error("successful retry did not complete inbox")
		}
		return err
	}))
}

func TestApplicationApprovalFailureRollsBackEveryPrizeBeforeRetry(t *testing.T) {
	h := setup(t, "final")
	ctx := context.Background()
	drawn, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.FinalDrawID, "test-admin")
	must(t, err)
	drawn, err = h.store.Draw(ctx, drawn.ID)
	must(t, err)
	if len(drawn.Selection.Winners) < 2 {
		t.Fatal("fixture must exercise failure after the first prize was queued")
	}
	fault := errors.New("injected second prize enqueue failure")
	repo := &enqueueFailpointRepository{Repository: h.store, failAt: 2, err: fault}
	service := app.New(repo, h.messenger, h.clock, firstCandidate{})
	_, err = service.ApproveDraw(ctx, h.run.CampaignID, drawn.ID, "test-admin")
	if !errors.Is(err, fault) || repo.calls != 2 {
		t.Fatalf("fault was not reached: calls=%d error=%v", repo.calls, err)
	}
	after, err := h.store.Draw(ctx, drawn.ID)
	must(t, err)
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	var slots, approvals int
	must(t, h.observer.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{h.name, "slots"}.Sanitize()+" WHERE draw_id=$1", drawn.ID).Scan(&slots))
	must(t, h.observer.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{h.name, "audit_events"}.Sanitize()+" WHERE campaign_id=$1 AND action=$2", h.run.CampaignID, "approve:"+drawn.ID).Scan(&approvals))
	if !reflect.DeepEqual(after, drawn) || len(h.offers(t, drawn.ID)) != 0 || len(messages) != 0 || slots != 0 || approvals != 0 {
		t.Fatalf("partial approval survived: draw=%+v messages=%+v slots=%d approvals=%d", after, messages, slots, approvals)
	}
	approved, err := service.ApproveDraw(ctx, h.run.CampaignID, drawn.ID, "test-admin")
	must(t, err)
	approved, err = h.store.Draw(ctx, approved.ID)
	must(t, err)
	if !reflect.DeepEqual(approved.Candidates, drawn.Candidates) || !reflect.DeepEqual(approved.Selection, drawn.Selection) || !reflect.DeepEqual(approved.DrawnAt, drawn.DrawnAt) {
		t.Fatal("approval retry changed the immutable draw snapshot")
	}
	offers := h.offers(t, drawn.ID)
	messages, err = h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if approved.State != domain.DrawApproved || len(offers) != len(drawn.Selection.Winners) || len(messages) != len(offers) {
		t.Fatalf("incomplete retry: draw=%+v offers=%+v messages=%+v", approved, offers, messages)
	}
	users := map[int64]bool{}
	for _, user := range drawn.Selection.Winners {
		users[user] = true
	}
	queued := map[string]int64{}
	for _, message := range messages {
		queued[message.OfferID] = message.UserID
	}
	for _, offer := range offers {
		if !users[offer.UserID] || queued[offer.ID] != offer.UserID || offer.Status != "pending" || offer.Deadline != nil {
			t.Fatalf("incorrect retried prize: %+v", offer)
		}
		delete(users, offer.UserID)
	}
	if len(users) != 0 {
		t.Fatalf("winners missing after retry: %v", users)
	}
	_, err = service.ApproveDraw(ctx, h.run.CampaignID, drawn.ID, "test-admin")
	must(t, err)
	after, err = h.store.Draw(ctx, drawn.ID)
	must(t, err)
	afterMessages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	if !reflect.DeepEqual(after, approved) || !reflect.DeepEqual(h.offers(t, drawn.ID), offers) || !reflect.DeepEqual(afterMessages, messages) {
		t.Fatal("repeated approval changed the saved result, offers or notifications")
	}
	must(t, h.observer.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{h.name, "slots"}.Sanitize()+" WHERE draw_id=$1", drawn.ID).Scan(&slots))
	must(t, h.observer.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{h.name, "audit_events"}.Sanitize()+" WHERE campaign_id=$1 AND action=$2", h.run.CampaignID, "approve:"+drawn.ID).Scan(&approvals))
	if slots != len(drawn.Selection.Winners) || approvals != 1 {
		t.Fatalf("retry/replay duplicated approval resources: slots=%d approvals=%d", slots, approvals)
	}
}
