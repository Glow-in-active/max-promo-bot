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

type campaignJourney struct {
	t             *testing.T
	h             *harness
	campaign      domain.Campaign
	days          []domain.Day
	userA         int64
	userB         int64
	expected      map[int64][]domain.Day
	originalPromo domain.Command
	originalCheck domain.Command
}

func TestCampaignJourneyBuildsTenAndSevenFinalWeightsFromRealActions(t *testing.T) {
	journey := newCampaignJourney(t)
	journey.completeDays()
	journey.assertPersonalSummaries()
	final := journey.runFinalDraw()
	journey.assertClosedCampaign(final)
}

func newCampaignJourney(t *testing.T) *campaignJourney {
	t.Helper()
	h := setup(t, "standard")
	ctx := context.Background()
	campaign, err := h.store.Campaign(ctx, h.run.CampaignID)
	must(t, err)
	var days []domain.Day
	must(t, h.store.Within(ctx, func(tx app.Tx) error {
		var err error
		days, err = tx.Days(ctx, campaign.ID)
		return err
	}))
	if len(days) != 10 {
		t.Fatalf("campaign has %d days", len(days))
	}
	userA, userB := h.run.UserIDs[0], h.run.UserIDs[1]
	return &campaignJourney{
		t:        t,
		h:        h,
		campaign: campaign,
		days:     days,
		userA:    userA,
		userB:    userB,
		expected: map[int64][]domain.Day{userA: {}, userB: {}},
	}
}

func (j *campaignJourney) execute(user int64, action string) domain.Message {
	j.t.Helper()
	command := j.h.command(action)
	command.UserID = user
	command.OccurredAt = j.h.clock.Now()
	if user == j.userA && action == "promo" && j.originalPromo.ID == "" {
		j.originalPromo = command
	}
	if user == j.userA && action == "check" && j.originalCheck.ID == "" {
		j.originalCheck = command
	}
	reply := j.h.execute(j.t, command)
	if action == "promo" && !strings.Contains(reply.Text, "Промокод:") {
		j.t.Fatalf("day %s user %d did not receive a code: %s", j.h.clock.Now(), user, reply.Text)
	}
	if action == "check" && !strings.Contains(reply.Text, "Накоплено шансов:") {
		j.t.Fatalf("day %s user %d was not registered: %s", j.h.clock.Now(), user, reply.Text)
	}
	return reply
}

func (j *campaignJourney) completeDays() {
	j.t.Helper()
	type participant struct {
		user    int64
		actions []string
	}
	modes := [][]string{{"check", "check"}, {"promo", "promo"}, {"promo", "check", "check", "promo"}}
	bDays := map[int]bool{1: true, 2: true, 3: true, 5: true, 7: true, 9: true, 10: true}
	for i, day := range j.days {
		j.h.clock.Set(day.StartsAt.Add(12 * time.Hour))
		participants := []participant{{j.userA, modes[i%len(modes)]}}
		if bDays[day.Number] {
			participants = append(participants, participant{j.userB, modes[(i+2)%len(modes)]})
		}
		for _, participant := range participants {
			j.expected[participant.user] = append(j.expected[participant.user], day)
			for _, action := range participant.actions {
				j.execute(participant.user, action)
				assertJourneyActivities(j.t, j.h, participant.user, j.expected[participant.user])
			}
		}
		assertJourneyActivities(j.t, j.h, j.userA, j.expected[j.userA])
		assertJourneyActivities(j.t, j.h, j.userB, j.expected[j.userB])
		if i == 0 {
			j.claimDailyPrize(day)
		}
	}
}

func (j *campaignJourney) claimDailyPrize(day domain.Day) {
	j.t.Helper()
	ctx := context.Background()
	j.h.clock.Set(day.EndsAt)
	draw, err := j.h.service.RunDraw(ctx, j.campaign.ID, day.DrawID, "journey-admin")
	must(j.t, err)
	if !reflect.DeepEqual(draw.Selection.Winners, []int64{j.userA}) {
		j.t.Fatalf("expected A to win the daily draw: %+v", draw.Selection)
	}
	_, err = j.h.service.ApproveDraw(ctx, j.campaign.ID, day.DrawID, "journey-admin")
	must(j.t, err)
	offers := j.h.offers(j.t, day.DrawID)
	if len(offers) != 1 || offers[0].UserID != j.userA {
		j.t.Fatalf("daily winner offer=%+v", offers)
	}
	must(j.t, j.h.service.MarkDelivered(ctx, offers[0].ID))
	claim := j.h.command("claim")
	claim.UserID = j.userA
	claim.OfferID = offers[0].ID
	claim.Name = "Алексей Победитель"
	claim.Consent = true
	j.h.execute(j.t, claim)
	claimed, err := j.h.store.Offer(ctx, offers[0].ID)
	must(j.t, err)
	if claimed.Status != "claimed" {
		j.t.Fatalf("daily prize was not claimed: %+v", claimed)
	}
	assertJourneyActivities(j.t, j.h, j.userA, j.expected[j.userA])
}

func (j *campaignJourney) assertPersonalSummaries() {
	j.t.Helper()
	for _, user := range []int64{j.userA, j.userB} {
		command := j.h.command("chances")
		command.UserID = user
		reply := j.h.execute(j.t, command)
		if !strings.Contains(reply.Text, fmt.Sprintf("Накоплено шансов: %d из 10", len(j.expected[user]))) {
			j.t.Fatalf("wrong chances for user %d: %s", user, reply.Text)
		}
		visited := make([]string, 0, len(j.expected[user]))
		for _, day := range j.expected[user] {
			visited = append(visited, fmt.Sprintf("день %d: %s", day.Number, day.Partner))
		}
		if !strings.Contains(reply.Text, "Пройденные дни: "+strings.Join(visited, ", ")+"\n") {
			j.t.Fatalf("wrong days or partners for user %d: %s", user, reply.Text)
		}
		if !strings.Contains(reply.Text, j.campaign.MainPrize) {
			j.t.Fatalf("main prize missing from personal summary: %s", reply.Text)
		}
	}
}

func (j *campaignJourney) runFinalDraw() domain.Draw {
	j.t.Helper()
	ctx := context.Background()
	j.h.clock.Set(j.campaign.EndsAt)
	final, err := j.h.service.RunDraw(ctx, j.campaign.ID, j.campaign.FinalDrawID, "journey-admin")
	must(j.t, err)
	wantCandidates := []domain.Candidate{{UserID: j.userA, Weight: 10}, {UserID: j.userB, Weight: 7}}
	if !reflect.DeepEqual(final.Candidates, wantCandidates) {
		j.t.Fatalf("final weights do not match actual activity: %+v", final.Candidates)
	}
	if !reflect.DeepEqual(final.Selection.Winners, []int64{j.userA, j.userB}) {
		j.t.Fatalf("daily winner A lost final eligibility: %+v", final.Selection)
	}
	return final
}

func (j *campaignJourney) assertClosedCampaign(final domain.Draw) {
	j.t.Helper()
	ctx := context.Background()
	j.h.clock.Set(j.campaign.EndsAt.Add(time.Hour))
	for _, saved := range []domain.Command{j.originalPromo, j.originalCheck} {
		replay := j.h.command(saved.Action)
		replay.UserID = saved.UserID
		replay.OccurredAt = saved.OccurredAt
		j.h.execute(j.t, replay)
	}
	for _, action := range []string{"promo", "check"} {
		executeLate := j.h.command(action)
		executeLate.UserID = j.userB
		executeLate.OccurredAt = j.h.clock.Now()
		if reply := j.h.execute(j.t, executeLate); !strings.Contains(reply.Text, "нет активного") {
			j.t.Fatalf("new action after campaign close was not rejected: %s", reply.Text)
		}
		executeLate.ID = j.h.command(action).ID
		executeLate.OccurredAt = j.days[3].StartsAt.Add(12 * time.Hour)
		j.h.execute(j.t, executeLate)
	}
	assertJourneyActivities(j.t, j.h, j.userA, j.expected[j.userA])
	assertJourneyActivities(j.t, j.h, j.userB, j.expected[j.userB])
	after, err := j.h.store.Draw(ctx, j.campaign.FinalDrawID)
	must(j.t, err)
	if !reflect.DeepEqual(after.Candidates, final.Candidates) || !reflect.DeepEqual(after.Selection, final.Selection) {
		j.t.Fatalf("late/replayed actions changed the saved final: before=%+v after=%+v", final, after)
	}
	replayedFinal, err := j.h.service.RunDraw(ctx, j.campaign.ID, j.campaign.FinalDrawID, "journey-admin")
	must(j.t, err)
	if !reflect.DeepEqual(replayedFinal, after) {
		j.t.Fatal("running the completed final changed its snapshot")
	}
}

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
