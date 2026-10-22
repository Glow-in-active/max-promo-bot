package postgres

import (
	"context"
	"encoding/json"
	"time"

	"promo/internal/domain"
)

const drawFields = `
	id, campaign_id, COALESCE(day_id, ''), kind, state, closes_at, winner_count,
	reserve_count, prize, claim_ttl_ns, candidates, selection, drawn_at, approved_at`

func scanDraw(row scanner) (domain.Draw, error) {
	var d domain.Draw
	var ttl int64
	var candidates, selection []byte
	err := row.Scan(
		&d.ID, &d.CampaignID, &d.DayID, &d.Kind, &d.State, &d.ClosesAt, &d.WinnerCount,
		&d.ReserveCount, &d.Prize, &ttl, &candidates, &selection, &d.DrawnAt, &d.ApprovedAt,
	)
	if err != nil {
		return d, missing(err, "Draw")
	}
	d.ClaimTTL = time.Duration(ttl)
	if err = json.Unmarshal(candidates, &d.Candidates); err != nil {
		return d, err
	}
	err = json.Unmarshal(selection, &d.Selection)
	return d, err
}

func (s *Store) Draw(ctx context.Context, id string) (domain.Draw, error) {
	return scanDraw(s.pool.QueryRow(ctx, "SELECT "+drawFields+" FROM draws WHERE id=$1", id))
}

func (t *transaction) Draw(ctx context.Context, id string, exclusive bool) (domain.Draw, error) {
	lock := " FOR SHARE"
	if exclusive {
		lock = " FOR UPDATE"
	}
	return scanDraw(t.tx.QueryRow(ctx, "SELECT "+drawFields+" FROM draws WHERE id=$1"+lock, id))
}

func (s *Store) Draws(ctx context.Context, campaign string) ([]domain.Draw, error) {
	return queryDraws(ctx, s.pool, "SELECT "+drawFields+`
		FROM draws
		WHERE campaign_id=$1
		ORDER BY closes_at, id`, campaign)
}

func queryDraws(ctx context.Context, q querier, query string, args ...any) ([]domain.Draw, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Draw{}
	for rows.Next() {
		d, e := scanDraw(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (t *transaction) Candidates(ctx context.Context, d domain.Draw) ([]domain.Candidate, error) {
	query := `SELECT user_id, 1 FROM entries WHERE draw_id=$1 ORDER BY user_id`
	key := d.ID
	if d.Kind == "final" {
		query = `
			SELECT user_id, count(*)::integer
			FROM activities
			WHERE campaign_id=$1
			GROUP BY user_id
			ORDER BY user_id`
		key = d.CampaignID
	}
	rows, err := t.tx.Query(ctx, query, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Candidate{}
	for rows.Next() {
		var c domain.Candidate
		if err = rows.Scan(&c.UserID, &c.Weight); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (t *transaction) SaveDraw(ctx context.Context, d domain.Draw) error {
	candidates, err := json.Marshal(d.Candidates)
	if err != nil {
		return err
	}
	selection, err := json.Marshal(d.Selection)
	if err != nil {
		return err
	}
	// A completed draw's participant snapshot and selection are write-once.
	tag, err := t.tx.Exec(ctx, `
		UPDATE draws
		SET state=$2,
		    candidates=CASE WHEN state='open' THEN $3::jsonb ELSE candidates END,
		    selection=CASE WHEN state='open' THEN $4::jsonb ELSE selection END,
		    drawn_at=COALESCE(drawn_at, $5),
		    approved_at=$6
		WHERE id=$1`, d.ID, d.State, candidates, selection, d.DrawnAt, d.ApprovedAt)
	return changed(tag, err, "Draw")
}

func (t *transaction) CreateSlot(ctx context.Context, id, draw string, position int) error {
	_, err := t.tx.Exec(ctx, "INSERT INTO slots(id,draw_id,position) VALUES($1,$2,$3)", id, draw, position)
	return err
}

func (t *transaction) LockSlot(ctx context.Context, id string) error {
	var found string
	err := t.tx.QueryRow(ctx, "SELECT id FROM slots WHERE id=$1 FOR UPDATE", id).Scan(&found)
	return missing(err, "Prize slot")
}

const offerFields = `
	id, slot_id, draw_id, user_id, status, sent_at, deadline, claimed_at, name, consent_at,
	COALESCE((SELECT state FROM outbox WHERE offer_id=offers.id ORDER BY seq DESC LIMIT 1), ''),
	COALESCE((SELECT last_error FROM outbox WHERE offer_id=offers.id ORDER BY seq DESC LIMIT 1), '')`

func scanOffer(row scanner) (domain.Offer, error) {
	var o domain.Offer
	err := row.Scan(
		&o.ID, &o.SlotID, &o.DrawID, &o.UserID, &o.Status, &o.SentAt,
		&o.Deadline, &o.ClaimedAt, &o.Name, &o.ConsentAt, &o.DeliveryState, &o.DeliveryError,
	)
	return o, missing(err, "Prize offer")
}

func (s *Store) Offer(ctx context.Context, id string) (domain.Offer, error) {
	return scanOffer(s.pool.QueryRow(ctx, "SELECT "+offerFields+" FROM offers WHERE id=$1", id))
}

func (t *transaction) Offer(ctx context.Context, id string) (domain.Offer, error) {
	return scanOffer(t.tx.QueryRow(ctx, "SELECT "+offerFields+" FROM offers WHERE id=$1", id))
}

func (t *transaction) Offers(ctx context.Context, draw string) ([]domain.Offer, error) {
	return queryOffers(ctx, t.tx, "SELECT "+offerFields+" FROM offers WHERE draw_id=$1 ORDER BY slot_id,id", draw)
}

func queryOffers(ctx context.Context, q querier, query string, args ...any) ([]domain.Offer, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Offer{}
	for rows.Next() {
		o, e := scanOffer(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (t *transaction) SaveOffer(ctx context.Context, o domain.Offer) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO offers(
			id, slot_id, draw_id, user_id, status, sent_at, deadline, claimed_at, name, consent_at
		)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT(id) DO UPDATE
		SET status=excluded.status,
		    sent_at=excluded.sent_at,
		    deadline=excluded.deadline,
		    claimed_at=excluded.claimed_at,
		    name=excluded.name,
		    consent_at=excluded.consent_at`,
		o.ID, o.SlotID, o.DrawID, o.UserID, o.Status,
		o.SentAt, o.Deadline, o.ClaimedAt, o.Name, o.ConsentAt,
	)
	return err
}

func (s *Store) DueDraws(ctx context.Context, now time.Time) ([]domain.Draw, error) {
	return queryDraws(ctx, s.pool, "SELECT "+drawFields+`
		FROM draws
		WHERE state='open' AND closes_at<=$1
		ORDER BY closes_at, id LIMIT 100`, now)
}

func (s *Store) DueOffers(ctx context.Context, now time.Time) ([]domain.Offer, error) {
	return queryOffers(ctx, s.pool, "SELECT "+offerFields+`
		FROM offers
		WHERE status='notified' AND deadline<=$1
		ORDER BY deadline, id LIMIT 100`, now)
}
