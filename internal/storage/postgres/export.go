package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"promo/internal/domain"
)

// Export reads the result, participant snapshot and current prize offers from
// one database snapshot, even when approval or reserve promotion happens concurrently.
func (s *Store) Export(ctx context.Context, campaignID string) (domain.ExportSnapshot, error) {
	var result domain.ExportSnapshot
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	result.Campaign, err = scanCampaign(tx.QueryRow(ctx, "SELECT "+campaignFields+" FROM campaigns WHERE id=$1", campaignID))
	if err != nil {
		return result, err
	}
	result.Draws, err = queryDraws(ctx, tx, "SELECT "+drawFields+" FROM draws WHERE campaign_id=$1 ORDER BY closes_at,id", campaignID)
	if err != nil {
		return result, err
	}
	result.Offers, err = queryOffers(ctx, tx, "SELECT "+offerFields+" FROM offers WHERE draw_id IN (SELECT id FROM draws WHERE campaign_id=$1) ORDER BY draw_id,slot_id,id", campaignID)
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}
