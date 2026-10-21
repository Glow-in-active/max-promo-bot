//go:build integration

package app_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
)

func assertJourneyActivities(t *testing.T, h *harness, user int64, want []domain.Day) {
	t.Helper()
	active, err := h.store.Activities(context.Background(), h.run.CampaignID, user)
	must(t, err)
	if len(active) != len(want) {
		t.Fatalf("user %d has %d chances, expected %d: %+v", user, len(active), len(want), active)
	}
	seen := map[string]bool{}
	for i, activity := range active {
		day := want[i]
		if seen[activity.DayID] || activity.DayID != day.ID || activity.DayNumber != day.Number || activity.Partner != day.Partner {
			t.Fatalf("user %d activity is duplicated or assigned to the wrong day/partner: %+v expected %+v", user, activity, day)
		}
		if activity.At.Before(day.StartsAt) || !activity.At.Before(day.EndsAt) {
			t.Fatalf("activity timestamp is outside its day: %+v", activity)
		}
		seen[activity.DayID] = true
	}
}

func TestCampaignJourneyBuildsTenAndSevenFinalWeightsFromRealActions(t *testing.T) {
	h := setup(t, "standard")
	ctx := context.Background()
	campaign, err := h.store.Campaign(ctx, h.run.CampaignID)
	must(t, err)
	var days []domain.Day
	must(t, h.store.Within(ctx, func(tx app.Tx) error { var err error; days, err = tx.Days(ctx, campaign.ID); return err }))
	if len(days) != 10 {
		t.Fatalf("campaign has %d days", len(days))
	}
	userA, userB := h.run.UserIDs[0], h.run.UserIDs[1]
	modes := [][]string{{"check", "check"}, {"promo", "promo"}, {"promo", "check", "check", "promo"}}
	bDays := map[int]bool{1: true, 2: true, 3: true, 5: true, 7: true, 9: true, 10: true}
	expected := map[int64][]domain.Day{userA: {}, userB: {}}
	var originalPromo, originalCheck domain.Command
	execute := func(user int64, action string) domain.Message {
		t.Helper()
		command := h.command(action)
		command.UserID = user
		command.OccurredAt = h.clock.Now()
		if user == userA && action == "promo" && originalPromo.ID == "" {
			originalPromo = command
		}
		if user == userA && action == "check" && originalCheck.ID == "" {
			originalCheck = command
		}
		reply := h.execute(t, command)
		if action == "promo" && !strings.Contains(reply.Text, "Промокод:") {
			t.Fatalf("day %s user %d did not receive a code: %s", h.clock.Now(), user, reply.Text)
		}
		if action == "check" && !strings.Contains(reply.Text, "Накоплено шансов:") {
			t.Fatalf("day %s user %d was not registered: %s", h.clock.Now(), user, reply.Text)
		}
		return reply
	}

	for i, day := range days {
		h.clock.Set(day.StartsAt.Add(12 * time.Hour))
		participants := []struct {
			user    int64
			actions []string
		}{{userA, modes[i%len(modes)]}}
		if bDays[day.Number] {
			participants = append(participants, struct {
				user    int64
				actions []string
			}{userB, modes[(i+2)%len(modes)]})
		}
		for _, participant := range participants {
			expected[participant.user] = append(expected[participant.user], day)
			for _, action := range participant.actions {
				execute(participant.user, action)
				assertJourneyActivities(t, h, participant.user, expected[participant.user])
			}
		}
		assertJourneyActivities(t, h, userA, expected[userA])
		assertJourneyActivities(t, h, userB, expected[userB])
		if i == 0 {
			h.clock.Set(day.EndsAt)
			draw, err := h.service.RunDraw(ctx, campaign.ID, day.DrawID, "journey-admin")
			must(t, err)
			if !reflect.DeepEqual(draw.Selection.Winners, []int64{userA}) {
				t.Fatalf("expected A to win the daily draw: %+v", draw.Selection)
			}
			_, err = h.service.ApproveDraw(ctx, campaign.ID, day.DrawID, "journey-admin")
			must(t, err)
			offers := h.offers(t, day.DrawID)
			if len(offers) != 1 || offers[0].UserID != userA {
				t.Fatalf("daily winner offer=%+v", offers)
			}
			must(t, h.service.MarkDelivered(ctx, offers[0].ID))
			claim := h.command("claim")
			claim.UserID = userA
			claim.OfferID = offers[0].ID
			claim.Name = "Алексей Победитель"
			claim.Consent = true
			h.execute(t, claim)
			claimed, err := h.store.Offer(ctx, offers[0].ID)
			must(t, err)
			if claimed.Status != "claimed" {
				t.Fatalf("daily prize was not claimed: %+v", claimed)
			}
			assertJourneyActivities(t, h, userA, expected[userA])
		}
	}

	for _, user := range []int64{userA, userB} {
		command := h.command("chances")
		command.UserID = user
		reply := h.execute(t, command)
		if !strings.Contains(reply.Text, fmt.Sprintf("Накоплено шансов: %d из 10", len(expected[user]))) {
			t.Fatalf("wrong chances for user %d: %s", user, reply.Text)
		}
		visited := make([]string, 0, len(expected[user]))
		for _, day := range expected[user] {
			visited = append(visited, fmt.Sprintf("день %d: %s", day.Number, day.Partner))
		}
		if !strings.Contains(reply.Text, "Пройденные дни: "+strings.Join(visited, ", ")+"\n") {
			t.Fatalf("wrong days or partners for user %d: %s", user, reply.Text)
		}
		if !strings.Contains(reply.Text, campaign.MainPrize) {
			t.Fatalf("main prize missing from personal summary: %s", reply.Text)
		}
	}
	h.clock.Set(campaign.EndsAt)
	final, err := h.service.RunDraw(ctx, campaign.ID, campaign.FinalDrawID, "journey-admin")
	must(t, err)
	wantCandidates := []domain.Candidate{{UserID: userA, Weight: 10}, {UserID: userB, Weight: 7}}
	if !reflect.DeepEqual(final.Candidates, wantCandidates) {
		t.Fatalf("final weights do not match actual activity: %+v", final.Candidates)
	}
	if !reflect.DeepEqual(final.Selection.Winners, []int64{userA, userB}) {
		t.Fatalf("daily winner A lost final eligibility: %+v", final.Selection)
	}

	h.clock.Set(campaign.EndsAt.Add(time.Hour))
	for _, saved := range []domain.Command{originalPromo, originalCheck} {
		replay := h.command(saved.Action)
		replay.UserID = saved.UserID
		replay.OccurredAt = saved.OccurredAt
		h.execute(t, replay)
	}
	for _, action := range []string{"promo", "check"} {
		executeLate := h.command(action)
		executeLate.UserID = userB
		executeLate.OccurredAt = h.clock.Now()
		if reply := h.execute(t, executeLate); !strings.Contains(reply.Text, "нет активного") {
			t.Fatalf("new action after campaign close was not rejected: %s", reply.Text)
		}
		executeLate.ID = h.command(action).ID
		executeLate.OccurredAt = days[3].StartsAt.Add(12 * time.Hour)
		h.execute(t, executeLate)
	}
	assertJourneyActivities(t, h, userA, expected[userA])
	assertJourneyActivities(t, h, userB, expected[userB])
	after, err := h.store.Draw(ctx, campaign.FinalDrawID)
	must(t, err)
	if !reflect.DeepEqual(after.Candidates, final.Candidates) || !reflect.DeepEqual(after.Selection, final.Selection) {
		t.Fatalf("late/replayed actions changed the saved final: before=%+v after=%+v", final, after)
	}
	replayedFinal, err := h.service.RunDraw(ctx, campaign.ID, campaign.FinalDrawID, "journey-admin")
	must(t, err)
	if !reflect.DeepEqual(replayedFinal, after) {
		t.Fatal("running the completed final changed its snapshot")
	}
}
