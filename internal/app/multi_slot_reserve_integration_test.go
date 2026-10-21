//go:build integration

package app_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"promo/internal/domain"
)

func TestApplicationConcurrentSlotsExhaustSharedReserveWithoutReusingPeople(t *testing.T) {
	h := setup(t, "final")
	ctx := context.Background()
	drawn, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.FinalDrawID, "test-admin")
	must(t, err)
	approved, err := h.service.ApproveDraw(ctx, h.run.CampaignID, drawn.ID, "test-admin")
	must(t, err)
	approved, err = h.store.Draw(ctx, approved.ID)
	must(t, err)
	originals := h.offers(t, drawn.ID)
	if len(originals) != 5 || len(drawn.Selection.Reserve) != 3 {
		t.Fatalf("fixture requires five prize slots and three shared reserve candidates: %+v", drawn.Selection)
	}
	for _, offer := range originals {
		must(t, h.service.MarkDelivered(ctx, offer.ID))
	}
	claim := h.command("claim")
	claim.UserID, claim.OfferID = originals[0].UserID, originals[0].ID
	claim.Name, claim.Consent = "Получатель Приза", true
	h.execute(t, claim)
	claimed, err := h.store.Offer(ctx, claim.OfferID)
	must(t, err)
	if claimed.Status != "claimed" || claimed.Deadline == nil || claimed.ConsentAt == nil {
		t.Fatalf("claim fixture did not complete: %+v", claimed)
	}
	h.clock.Set(*claimed.Deadline)
	concurrently(t, 40, func(i int) error {
		return h.service.ExpireOffer(ctx, originals[i%len(originals)].ID)
	})
	offers := h.offers(t, drawn.ID)
	if len(offers) != 8 {
		t.Fatalf("expected five original and three reserve offers, got %+v", offers)
	}
	originalByID := map[string]domain.Offer{}
	replacementsBySlot := map[string]int{}
	for _, offer := range originals {
		originalByID[offer.ID] = offer
		replacementsBySlot[offer.SlotID] = 0
	}
	remainingReserve := map[int64]bool{}
	for _, user := range drawn.Selection.Reserve {
		remainingReserve[user] = true
	}
	users := map[int64]bool{}
	var reserves []domain.Offer
	for _, offer := range offers {
		if users[offer.UserID] {
			t.Fatalf("person received more than one offer in the same draw: %+v", offer)
		}
		users[offer.UserID] = true
		if _, original := originalByID[offer.ID]; original {
			if offer.ID == claimed.ID {
				if !reflect.DeepEqual(offer, claimed) {
					t.Fatalf("expiry changed the claimed prize or consent: before=%+v after=%+v", claimed, offer)
				}
			} else if offer.Status != "expired" {
				t.Fatalf("unclaimed original survived its deadline: %+v", offer)
			}
			continue
		}
		if _, exists := replacementsBySlot[offer.SlotID]; !exists || offer.SlotID == claimed.SlotID || !remainingReserve[offer.UserID] {
			t.Fatalf("replacement is outside the approved slots/reserve: %+v", offer)
		}
		delete(remainingReserve, offer.UserID)
		replacementsBySlot[offer.SlotID]++
		if replacementsBySlot[offer.SlotID] > 1 || offer.Status != "pending" || offer.SentAt != nil || offer.Deadline != nil {
			t.Fatalf("invalid simultaneous replacement: %+v", offer)
		}
		reserves = append(reserves, offer)
	}
	if len(remainingReserve) != 0 || len(reserves) != 3 {
		t.Fatalf("shared reserve was not consumed completely: unused=%v offers=%+v", remainingReserve, reserves)
	}
	var exhaustedSlot string
	for slot, count := range replacementsBySlot {
		if slot != claimed.SlotID && count == 0 {
			if exhaustedSlot != "" {
				t.Fatal("more than one unclaimed slot lost its replacement")
			}
			exhaustedSlot = slot
		}
	}
	if exhaustedSlot == "" {
		t.Fatal("fixture did not exhaust the reserve")
	}
	var exhaustionEvents int
	must(t, h.observer.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{h.name, "audit_events"}.Sanitize()+" WHERE campaign_id=$1 AND action=$2", h.run.CampaignID, "reserve_exhausted:"+exhaustedSlot).Scan(&exhaustionEvents))
	if exhaustionEvents != 1 {
		t.Fatalf("duplicate expiry changed the exhaustion audit: count=%d", exhaustionEvents)
	}
	messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	prizeMessages := map[string]int64{}
	for _, message := range messages {
		if message.OfferID == "" {
			continue
		}
		if _, duplicate := prizeMessages[message.OfferID]; duplicate {
			t.Fatalf("duplicate prize notification: %+v", message)
		}
		prizeMessages[message.OfferID] = message.UserID
	}
	if len(prizeMessages) != len(offers) {
		t.Fatalf("prize notifications=%d offers=%d", len(prizeMessages), len(offers))
	}
	for _, offer := range offers {
		if prizeMessages[offer.ID] != offer.UserID {
			t.Fatalf("prize notification has incorrect recipient: %+v", offer)
		}
	}

	// Expired reserve recipients must remain used when other slots need a replacement.
	for _, offer := range reserves {
		must(t, h.service.MarkDelivered(ctx, offer.ID))
	}
	reserve, err := h.store.Offer(ctx, reserves[0].ID)
	must(t, err)
	if reserve.Deadline == nil {
		t.Fatal("reserve delivery did not start the response period")
	}
	h.clock.Set(*reserve.Deadline)
	concurrently(t, 40, func(i int) error {
		return h.service.ExpireOffer(ctx, offers[i%len(offers)].ID)
	})
	after := h.offers(t, drawn.ID)
	if len(after) != len(offers) {
		t.Fatalf("exhausted reserve produced extra offers: %+v", after)
	}
	for _, offer := range after {
		if offer.ID == claimed.ID {
			if !reflect.DeepEqual(offer, claimed) {
				t.Fatal("reserve expiry changed the claimed prize")
			}
		} else if offer.Status != "expired" {
			t.Fatalf("offer survived the second expiry wave: %+v", offer)
		}
	}
	afterMessages, err := h.store.Messages(ctx, h.run.CampaignID, "")
	must(t, err)
	afterDraw, err := h.store.Draw(ctx, drawn.ID)
	must(t, err)
	if !reflect.DeepEqual(messages, afterMessages) || !reflect.DeepEqual(approved, afterDraw) {
		t.Fatal("reserve exhaustion changed the approved snapshot or queued new notifications")
	}
}
