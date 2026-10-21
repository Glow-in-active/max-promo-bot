//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"promo/internal/domain"
)

func TestSetupRejectsCalendarDayConflictsBeforeImport(t *testing.T) {
	for _, scenario := range []string{"two periods on one local date", "period crosses local midnight"} {
		t.Run(scenario, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			data, err := os.ReadFile("../../../config/campaign.example.json")
			requireNoError(t, err)
			var setup domain.Setup
			requireNoError(t, json.Unmarshal(data, &setup))
			setup, err = setup.Prepare()
			requireNoError(t, err)
			start := setup.Days[0].StartsAt
			if scenario == "two periods on one local date" {
				setup.Days[0].EndsAt = start.Add(2 * time.Hour)
				setup.Days[1].StartsAt = start.Add(4 * time.Hour)
				setup.Days[1].EndsAt = start.Add(6 * time.Hour)
			} else {
				setup.Days[0].StartsAt = start.Add(23 * time.Hour)
				setup.Days[0].EndsAt = start.Add(25 * time.Hour)
				setup.Days[1].StartsAt = start.Add(27 * time.Hour)
			}
			for i := range setup.Draws {
				for _, day := range setup.Days {
					if setup.Draws[i].ID == day.DrawID {
						setup.Draws[i].ClosesAt = day.EndsAt
					}
				}
			}
			err = s.Setup(ctx, setup)
			fault, ok := domain.FaultOf(err)
			if !ok || fault.Code != domain.Invalid {
				t.Fatalf("invalid calendar import error = %v", err)
			}
			var rows int
			requireNoError(t, s.pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM campaigns) +
				(SELECT count(*) FROM days) +
				(SELECT count(*) FROM draws) +
				(SELECT count(*) FROM promo_pools) +
				(SELECT count(*) FROM promos)`).Scan(&rows))
			if rows != 0 {
				t.Fatalf("rejected calendar import left %d rows", rows)
			}
		})
	}
}
