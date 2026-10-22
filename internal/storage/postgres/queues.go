package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"promo/internal/domain"
)

func (t *transaction) Enqueue(ctx context.Context, m domain.Message, dedupKey string, now time.Time) error {
	buttons, err := json.Marshal(m.Buttons)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `
		INSERT INTO outbox(
			id, campaign_id, user_id, text, buttons, offer_id, banner, next_attempt_at, created_at, dedup_key
		)
		VALUES($1, $2, $3, $4, $5, $6, $7, $8, $8, $9)
		ON CONFLICT(dedup_key) DO NOTHING`,
		m.ID, m.CampaignID, m.UserID, m.Text, buttons,
		nullable(m.OfferID), m.Banner, now, dedupKey,
	)
	return err
}

func (s *Store) PutInbox(ctx context.Context, c domain.Command, key string) (bool, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return false, err
	}
	id := c.ID
	if id == "" {
		id = newID()
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO inbox(id, dedup_key, command)
		VALUES($1, $2, $3)
		ON CONFLICT(dedup_key) DO NOTHING`, id, key, payload)
	return tag.RowsAffected() > 0, err
}

func (t *transaction) InboxDone(ctx context.Context, id string) (bool, error) {
	var done bool
	// A recovered lease can overlap an old worker. Locking the event serializes
	// their completion checks with the business mutation in this transaction.
	err := t.tx.QueryRow(ctx, "SELECT state='done' FROM inbox WHERE id=$1 FOR UPDATE", id).Scan(&done)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return done, err
}

func (t *transaction) CompleteInbox(ctx context.Context, id string) error {
	_, err := t.tx.Exec(ctx, "UPDATE inbox SET state='done',lease_until=NULL,last_error='' WHERE id=$1", id)
	return err
}

func (s *Store) TakeInbox(ctx context.Context, now time.Time) (*domain.Event, error) {
	var event domain.Event
	var payload []byte
	err := s.pool.QueryRow(ctx, `
		WITH ready AS (
			SELECT id FROM inbox
			WHERE (state IN ('pending', 'retry') AND next_attempt_at <= $1)
			   OR (state = 'processing' AND lease_until <= $1)
			ORDER BY seq LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE inbox
		SET state = 'processing', attempts = attempts + 1,
		    lease_until = $1 + interval '30 seconds'
		FROM ready WHERE inbox.id = ready.id
		RETURNING inbox.id, inbox.command, inbox.attempts`, now,
	).Scan(&event.ID, &payload, &event.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(payload, &event.Command); err != nil {
		return nil, fmt.Errorf("decode inbox %s: %w", event.ID, err)
	}
	event.Command.ID = event.ID
	return &event, nil
}

func (s *Store) RetryInbox(ctx context.Context, id, reason string, next time.Time, terminal bool) error {
	state := "retry"
	if terminal {
		state = "failed"
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE inbox
		SET state=$2, next_attempt_at=$3, lease_until=NULL, last_error=$4
		WHERE id=$1 AND state<>'done'`, id, state, next, reason)
	return changed(tag, err, "Event")
}

const messageFields = `id, campaign_id, user_id, text, buttons, COALESCE(offer_id, ''), banner, state, attempts`

func scanMessage(row scanner) (domain.Message, error) {
	var m domain.Message
	var buttons []byte
	err := row.Scan(
		&m.ID, &m.CampaignID, &m.UserID, &m.Text, &buttons,
		&m.OfferID, &m.Banner, &m.State, &m.Attempts,
	)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(buttons, &m.Buttons)
	return m, err
}

const recipientLockKey = `hashtextextended('promo.outbox:' || $1::bigint::text, 0)`

const eligibleMessage = `
	((candidate.state IN ('pending', 'retry') AND candidate.next_attempt_at <= $1)
	 OR (candidate.state = 'processing' AND candidate.lease_until <= $1))
	AND NOT EXISTS (
		SELECT 1 FROM outbox AS earlier
		WHERE earlier.user_id = candidate.user_id AND earlier.seq < candidate.seq
		  AND earlier.state IN ('pending', 'retry', 'processing')
	)
	AND NOT EXISTS (
		SELECT 1 FROM outbox AS active
		WHERE active.user_id = candidate.user_id AND active.id <> candidate.id
		  AND active.state = 'processing' AND active.lease_until > $1
	)`

func (s *Store) TakeMessage(ctx context.Context, now time.Time) (*domain.Message, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		SELECT candidate.user_id FROM outbox AS candidate
		WHERE `+eligibleMessage+`
		ORDER BY candidate.seq LIMIT 32`, now)
	if err != nil {
		return nil, err
	}
	users, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		var locked bool
		if err := tx.QueryRow(ctx,
			"SELECT pg_try_advisory_xact_lock("+recipientLockKey+")", user,
		).Scan(&locked); err != nil {
			return nil, err
		}
		if !locked {
			continue
		}
		message, err := scanMessage(tx.QueryRow(ctx, `
			WITH ready AS (
				SELECT candidate.id FROM outbox AS candidate
				WHERE candidate.user_id = $2 AND `+eligibleMessage+`
				ORDER BY candidate.seq LIMIT 1 FOR UPDATE OF candidate SKIP LOCKED
			)
			UPDATE outbox
			SET state = 'processing', attempts = attempts + 1,
			    lease_until = $1 + interval '30 seconds'
			FROM ready WHERE outbox.id = ready.id
			RETURNING outbox.id, outbox.campaign_id, outbox.user_id, outbox.text,
			          outbox.buttons, COALESCE(outbox.offer_id, ''), outbox.banner,
			          outbox.state, outbox.attempts`, now, user,
		))
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &message, nil
	}
	return nil, nil
}

func (s *Store) FinishMessage(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, "UPDATE outbox SET state='delivered',lease_until=NULL,last_error='' WHERE id=$1", id)
	return changed(tag, err, "Message")
}

func (s *Store) RetryMessage(ctx context.Context, id, reason string, next time.Time, terminal bool) error {
	state := "retry"
	if terminal {
		state = "failed"
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox
		SET state=$2, next_attempt_at=$3, lease_until=NULL, last_error=$4
		WHERE id=$1 AND state<>'delivered'`, id, state, next, reason)
	return changed(tag, err, "Message")
}

// Messages exposes message IDs as cursors while keeping ordering monotonic via seq.
func (s *Store) Messages(ctx context.Context, campaign, cursor string) ([]domain.Message, error) {
	after := int64(0)
	if cursor != "" {
		if err := s.pool.QueryRow(ctx,
			"SELECT seq FROM outbox WHERE id=$1 AND campaign_id=$2", cursor, campaign,
		).Scan(&after); err != nil {
			return nil, missing(err, "Message cursor")
		}
	}
	rows, err := s.pool.Query(ctx, "SELECT "+messageFields+`
		FROM outbox
		WHERE campaign_id=$1 AND seq>$2
		ORDER BY seq LIMIT 200`, campaign, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Message{}
	for rows.Next() {
		m, e := scanMessage(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (t *transaction) RetryOfferMessage(ctx context.Context, offer string, now time.Time) error {
	var user int64
	err := t.tx.QueryRow(ctx, "SELECT user_id FROM offers WHERE id=$1", offer).Scan(&user)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Fail(domain.InvalidTransition, "No undelivered notification is available for retry.")
	}
	if err != nil {
		return err
	}
	if _, err := t.tx.Exec(ctx, "SELECT pg_advisory_xact_lock("+recipientLockKey+")", user); err != nil {
		return err
	}
	tag, err := t.tx.Exec(ctx, `
		UPDATE outbox
		SET state='pending', attempts=0, next_attempt_at=$2, lease_until=NULL, last_error=''
		WHERE offer_id=$1 AND state='failed'`, offer, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.Fail(domain.InvalidTransition, "No undelivered notification is available for retry.")
	}
	return nil
}
