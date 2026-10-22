package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"promo/internal/domain"
)

func (s *Store) Setup(ctx context.Context, setup domain.Setup) error {
	if err := domain.ValidateSetup(setup); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin campaign import: %w", err)
	}
	defer tx.Rollback(ctx)
	c := setup.Campaign
	_, err = tx.Exec(ctx, `
		INSERT INTO campaigns(
			id, name, status, starts_at, ends_at, timezone, final_draw_id, main_prize, rules
		)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		c.ID, c.Name, c.Status, c.StartsAt, c.EndsAt,
		c.Timezone, c.FinalDrawID, c.MainPrize, c.Rules,
	)
	if err != nil {
		var databaseError *pgconn.PgError
		if errors.As(err, &databaseError) && databaseError.Code == "23505" &&
			databaseError.ConstraintName == "campaigns_pkey" {
			return domain.Fail(domain.InvalidTransition, "Campaign ID already exists. Import made no changes.")
		}
		return fmt.Errorf("insert campaign: %w", err)
	}
	for _, d := range setup.Days {
		_, err = tx.Exec(ctx, `
			INSERT INTO days(
				id, campaign_id, number, starts_at, ends_at,
				partner, partner_url, channel_id, instruction, draw_id
			)
			VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			d.ID, d.CampaignID, d.Number, d.StartsAt, d.EndsAt,
			d.Partner, d.PartnerURL, d.ChannelID, d.Instruction, d.DrawID,
		)
		if err != nil {
			return fmt.Errorf("insert day %s: %w", d.ID, err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO promo_pools(day_id) VALUES($1)", d.ID); err != nil {
			return fmt.Errorf("insert pool %s: %w", d.ID, err)
		}
	}
	for _, d := range setup.Draws {
		_, err = tx.Exec(ctx, `
			INSERT INTO draws(
				id, campaign_id, day_id, kind, state,
				closes_at, winner_count, reserve_count, prize, claim_ttl_ns
			)
			VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			d.ID, d.CampaignID, nullable(d.DayID), d.Kind, d.State,
			d.ClosesAt, d.WinnerCount, d.ReserveCount, d.Prize, int64(d.ClaimTTL),
		)
		if err != nil {
			return fmt.Errorf("insert draw %s: %w", d.ID, err)
		}
	}
	if len(setup.Promos) > 0 {
		rows := make([][]any, 0, len(setup.Promos))
		for _, p := range setup.Promos {
			rows = append(rows, []any{p.ID, p.DayID, p.Value})
		}
		if _, err = tx.CopyFrom(
			ctx, pgx.Identifier{"promos"}, []string{"id", "day_id", "value"}, pgx.CopyFromRows(rows),
		); err != nil {
			return fmt.Errorf("import promo pool: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit campaign import: %w", err)
	}
	return nil
}
