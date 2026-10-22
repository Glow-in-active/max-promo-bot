package postgres

import (
	"context"

	"promo/internal/domain"
)

func (s *Store) Stats(ctx context.Context, campaign string) (domain.Stats, error) {
	result := domain.Stats{NewUsersByDay: map[string]int{}, PromoRemaining: []domain.PartnerPromoStock{}, Entries: map[string]int{}}
	// One read-only snapshot keeps counters mutually consistent during live traffic.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return result, err
	}
	var zone string
	if err = tx.QueryRow(ctx, "SELECT timezone FROM campaigns WHERE id=$1", campaign).Scan(&zone); err != nil {
		return result, missing(err, "Campaign")
	}
	err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM campaign_users WHERE campaign_id=$1),(SELECT count(*) FROM promo_requests WHERE campaign_id=$1),(SELECT count(*) FROM promos p JOIN days d ON d.id=p.day_id WHERE d.campaign_id=$1 AND p.user_id IS NOT NULL),(SELECT count(*) FROM outbox WHERE campaign_id=$1 AND state='failed')`, campaign).Scan(&result.Users, &result.PromoRequests, &result.PromoIssued, &result.FailedMessages)
	if err != nil {
		return result, err
	}
	queries := []struct {
		sql    string
		args   []any
		target map[string]int
	}{
		{`SELECT to_char(started_at AT TIME ZONE $2,'YYYY-MM-DD'),count(*) FROM campaign_users WHERE campaign_id=$1 GROUP BY 1`, []any{campaign, zone}, result.NewUsersByDay},
		{`SELECT d.id,CASE WHEN d.kind='final' THEN (SELECT count(DISTINCT user_id) FROM activities WHERE campaign_id=$1) ELSE (SELECT count(*) FROM entries WHERE draw_id=d.id) END FROM draws d WHERE d.campaign_id=$1`, []any{campaign}, result.Entries},
	}
	for _, query := range queries {
		rows, e := tx.Query(ctx, query.sql, query.args...)
		if e != nil {
			return result, e
		}
		for rows.Next() {
			var key string
			var count int
			if e = rows.Scan(&key, &count); e != nil {
				rows.Close()
				return result, e
			}
			query.target[key] = count
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return result, e
		}
	}
	result.PromoRemaining, err = partnerPromoStock(ctx, tx, campaign)
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

func partnerPromoStock(ctx context.Context, query querier, campaign string) ([]domain.PartnerPromoStock, error) {
	rows, err := query.Query(ctx, `
SELECT d.channel_id, d.partner, d.id, d.number,
       count(p.id) FILTER (WHERE p.user_id IS NULL)
FROM days d LEFT JOIN promos p ON p.day_id = d.id
WHERE d.campaign_id = $1
GROUP BY d.id
ORDER BY d.number`, campaign)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.PartnerPromoStock{}
	indices := map[int64]int{}
	for rows.Next() {
		var channelID int64
		var partner string
		var day domain.DayPromoStock
		if err = rows.Scan(&channelID, &partner, &day.DayID, &day.DayNumber, &day.Remaining); err != nil {
			return nil, err
		}
		index, exists := indices[channelID]
		if !exists {
			index = len(result)
			indices[channelID] = index
			result = append(result, domain.PartnerPromoStock{ChannelID: channelID, Partner: partner, Days: []domain.DayPromoStock{}})
		}
		result[index].Remaining += day.Remaining
		result[index].Days = append(result[index].Days, day)
	}
	return result, rows.Err()
}
