//go:build integration

package app_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
)

func TestApplicationMenuAndInformation(t *testing.T) {
	h := setup(t, "standard")
	for _, action := range []string{"start", "help", "rules", "chances", "participate", "claim_name"} {
		reply := h.execute(t, h.command(action))
		if reply.Text == "" || len(reply.Buttons) == 0 {
			t.Fatalf("%s has no response or navigation", action)
		}
		if action == "start" && (!reply.Banner || len(reply.Buttons) != 5) {
			t.Fatal("start must show banner and five actions")
		}
	}
}

func TestApplicationClaimConversationSurvivesReplayedButtons(t *testing.T) {
	h := setup(t, "drawable")
	ctx := context.Background()
	_, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	_, err = h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	offer := h.offers(t, h.run.DrawID)[0]
	command := func(action string) domain.Command {
		c := h.command(action)
		c.UserID = offer.UserID
		c.OfferID = offer.ID
		return c
	}
	h.execute(t, command("claim_begin"))
	first, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if first.SentAt == nil || first.Deadline == nil {
		t.Fatal("verified owner click did not acknowledge delivery")
	}
	name := command("claim_name")
	name.Name = "Иван"
	if !strings.Contains(h.execute(t, name).Text, "имя и фамилию") {
		t.Fatal("one-part name accepted")
	}
	name = command("claim_name")
	name.Name = "Иван Петров"
	h.execute(t, name)
	h.clock.Set(h.clock.Now().Add(time.Minute))
	repeated := h.execute(t, command("claim_begin"))
	if len(repeated.Buttons) != 1 || repeated.Buttons[0].Payload != "claim:"+offer.ID {
		t.Fatal("replayed begin lost consent step")
	}
	claim := command("claim")
	h.execute(t, claim)
	stillPending, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if stillPending.Status == "claimed" {
		t.Fatal("claim without consent accepted")
	}
	claim = command("claim")
	claim.Consent = true
	h.execute(t, claim)
	claimed, err := h.store.Offer(ctx, offer.ID)
	must(t, err)
	if claimed.Status != "claimed" || claimed.Name != "Иван Петров" || claimed.ConsentAt == nil || !claimed.Deadline.Equal(*first.Deadline) {
		t.Fatalf("claim or fixed deadline lost: %+v", claimed)
	}
	if !strings.Contains(h.execute(t, command("claim_begin")).Text, "уже подтверждено") {
		t.Fatal("claimed begin was not idempotent")
	}
	h.execute(t, claim)
	listed, err := h.service.ListOffers(ctx, h.run.CampaignID, h.run.DrawID)
	must(t, err)
	if len(listed) == 0 {
		t.Fatal("operator cannot inspect offers")
	}
}

func TestApplicationWorkerDrainsDurableInboxAndStops(t *testing.T) {
	h := setup(t, "standard")
	ctx, cancel := context.WithCancel(context.Background())
	// Queue availability uses database wall time, so this lifecycle test uses the
	// real clock. Boundary tests inject a clock and explicitly coordinate timestamps.
	run, err := h.store.CreateDemo(ctx, "standard", time.Now().UTC())
	must(t, err)
	h.run = run
	h.service = app.New(h.store, h.messenger, app.RealClock{}, firstCandidate{})
	command := h.command("promo")
	_, err = h.store.PutInbox(ctx, command, command.ID)
	must(t, err)
	done := make(chan struct{})
	go func() { defer close(done); h.service.Work(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		messages, err := h.store.Messages(ctx, h.run.CampaignID, "")
		must(t, err)
		if len(messages) == 1 && messages[0].State == "delivered" {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("durable inbox/outbox did not drain")
		case <-tick.C:
		}
	}
}

func TestExportSnapshotCannotMixApprovalStates(t *testing.T) {
	h := setup(t, "drawable")
	ctx := context.Background()
	_, err := h.service.RunDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
	must(t, err)
	concurrently(t, 20, func(i int) error {
		if i == 0 {
			_, err := h.service.ApproveDraw(ctx, h.run.CampaignID, h.run.DrawID, "admin")
			return err
		}
		snapshot, err := h.store.Export(ctx, h.run.CampaignID)
		if err != nil {
			return err
		}
		for _, offer := range snapshot.Offers {
			for _, draw := range snapshot.Draws {
				if draw.ID == offer.DrawID && draw.State != domain.DrawApproved {
					t.Errorf("offer %s read with unapproved result", offer.ID)
				}
			}
		}
		return nil
	})
	_, err = h.store.Export(ctx, "missing-campaign")
	if f, ok := domain.FaultOf(err); !ok || f.Code != domain.NotFound {
		t.Fatal("missing campaign should fail explicitly", err)
	}
}
