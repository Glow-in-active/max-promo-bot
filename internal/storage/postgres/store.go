package postgres

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"promo/internal/app"
	"promo/internal/domain"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Store struct{ pool *pgxpool.Pool }
type transaction struct{ tx pgx.Tx }
type scanner interface{ Scan(...any) error }
type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

var _ app.Repository = (*Store)(nil)
var _ app.Tx = (*transaction)(nil)

func New(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration: %w", err)
	}
	config.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(824125891)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return err
	}
	files, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, name := range files {
		var exists bool
		if err = tx.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version=$1)", name,
		).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		source, readErr := migrationFiles.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		if _, err = tx.Exec(ctx, string(source)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES ($1)", name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Within retries only failures for which PostgreSQL guarantees a rollback.
// Callbacks must keep all external side effects outside the transaction.
func (s *Store) Within(ctx context.Context, fn func(app.Tx) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		err := s.once(ctx, fn)
		if err == nil || !retryable(err) || attempt == 2 {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	panic("unreachable transaction retry")
}

func (s *Store) once(ctx context.Context, fn func(app.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err = fn(&transaction{tx: tx}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func retryable(err error) bool {
	var p *pgconn.PgError
	return errors.As(err, &p) && (p.Code == "40001" || p.Code == "40P01")
}

func missing(err error, subject string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Fail(domain.NotFound, subject+" not found.")
	}
	return err
}

func changed(tag pgconn.CommandTag, err error, subject string) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.Fail(domain.NotFound, subject+" not found.")
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

const campaignFields = `id, name, status, starts_at, ends_at, timezone, final_draw_id, main_prize, rules`

func scanCampaign(row scanner) (domain.Campaign, error) {
	var c domain.Campaign
	err := row.Scan(
		&c.ID, &c.Name, &c.Status, &c.StartsAt, &c.EndsAt,
		&c.Timezone, &c.FinalDrawID, &c.MainPrize, &c.Rules,
	)
	return c, missing(err, "Campaign")
}

func (s *Store) Campaign(ctx context.Context, id string) (domain.Campaign, error) {
	return scanCampaign(s.pool.QueryRow(ctx, "SELECT "+campaignFields+" FROM campaigns WHERE id=$1", id))
}

func (t *transaction) Campaign(ctx context.Context, id string, exclusive bool) (domain.Campaign, error) {
	lock := " FOR SHARE"
	if exclusive {
		lock = " FOR UPDATE"
	}
	return scanCampaign(t.tx.QueryRow(ctx, "SELECT "+campaignFields+" FROM campaigns WHERE id=$1"+lock, id))
}

func (t *transaction) SetCampaignClosed(ctx context.Context, id string) error {
	tag, err := t.tx.Exec(ctx, "UPDATE campaigns SET status='closed' WHERE id=$1", id)
	return changed(tag, err, "Campaign")
}

const dayFields = `id, campaign_id, number, starts_at, ends_at, partner, partner_url, channel_id, instruction, draw_id`

func scanDay(row scanner) (domain.Day, error) {
	var d domain.Day
	err := row.Scan(
		&d.ID, &d.CampaignID, &d.Number, &d.StartsAt, &d.EndsAt,
		&d.Partner, &d.PartnerURL, &d.ChannelID, &d.Instruction, &d.DrawID,
	)
	return d, missing(err, "Campaign day")
}

func dayAt(ctx context.Context, q querier, campaign string, now time.Time) (domain.Day, error) {
	return scanDay(q.QueryRow(ctx, "SELECT "+dayFields+`
		FROM days
		WHERE campaign_id=$1 AND starts_at<=$2 AND ends_at>$2`, campaign, now,
	))
}

func (s *Store) CurrentDay(ctx context.Context, c string, now time.Time) (domain.Day, error) {
	return dayAt(ctx, s.pool, c, now)
}

func (t *transaction) DayAt(ctx context.Context, c string, now time.Time) (domain.Day, error) {
	return dayAt(ctx, t.tx, c, now)
}

func (t *transaction) Days(ctx context.Context, c string) ([]domain.Day, error) {
	rows, err := t.tx.Query(ctx, "SELECT "+dayFields+" FROM days WHERE campaign_id=$1 ORDER BY number", c)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.Day{}
	for rows.Next() {
		d, e := scanDay(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (t *transaction) UserForUpdate(ctx context.Context, id int64, now time.Time) (domain.User, error) {
	if _, err := t.tx.Exec(ctx, `
		INSERT INTO users(id, started_at)
		VALUES($1, $2)
		ON CONFLICT(id) DO NOTHING`, id, now); err != nil {
		return domain.User{}, err
	}
	var u domain.User
	err := t.tx.QueryRow(ctx, "SELECT id,started_at FROM users WHERE id=$1 FOR UPDATE", id).Scan(&u.ID, &u.StartedAt)
	return u, err
}

func (t *transaction) TouchCampaignUser(ctx context.Context, c string, u int64, now time.Time) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO campaign_users(campaign_id, user_id, started_at)
		VALUES($1, $2, $3)
		ON CONFLICT DO NOTHING`, c, u, now)
	return err
}

func activities(ctx context.Context, q querier, c string, u int64) ([]domain.Activity, error) {
	rows, err := q.Query(ctx, `
		SELECT a.day_id, d.number, d.partner, a.created_at
		FROM activities a JOIN days d ON d.id=a.day_id
		WHERE a.campaign_id=$1 AND a.user_id=$2
		ORDER BY d.number`, c, u)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Activity{}
	for rows.Next() {
		var a domain.Activity
		if err = rows.Scan(&a.DayID, &a.DayNumber, &a.Partner, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Activities(ctx context.Context, c string, u int64) ([]domain.Activity, error) {
	return activities(ctx, s.pool, c, u)
}

func (t *transaction) Activities(ctx context.Context, c string, u int64) ([]domain.Activity, error) {
	return activities(ctx, t.tx, c, u)
}

func (t *transaction) AddActivity(
	ctx context.Context, c string, u int64, d domain.Day, source string, now time.Time,
) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO activities(campaign_id, user_id, day_id, source, created_at)
		VALUES($1, $2, $3, $4, $5)`, c, u, d.ID, source, now)
	return err
}

func (t *transaction) Assignment(ctx context.Context, day string, u int64) (*domain.Promo, error) {
	var p domain.Promo
	err := t.tx.QueryRow(ctx, `
		SELECT id, day_id, value, user_id
		FROM promos
		WHERE day_id=$1 AND user_id=$2`, day, u,
	).Scan(&p.ID, &p.DayID, &p.Value, &p.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (t *transaction) LockPool(ctx context.Context, day string) error {
	var id string
	err := t.tx.QueryRow(ctx, "SELECT day_id FROM promo_pools WHERE day_id=$1 FOR UPDATE", day).Scan(&id)
	return missing(err, "Promo pool")
}

func (t *transaction) NextPromo(ctx context.Context, day string) (*domain.Promo, error) {
	var p domain.Promo
	err := t.tx.QueryRow(ctx, `
		SELECT id, day_id, value
		FROM promos
		WHERE day_id=$1 AND user_id IS NULL
		ORDER BY id LIMIT 1 FOR UPDATE`, day,
	).Scan(&p.ID, &p.DayID, &p.Value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (t *transaction) AssignPromo(ctx context.Context, id string, u int64, now time.Time) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE promos
		SET user_id=$2, assigned_at=$3
		WHERE id=$1 AND user_id IS NULL`, id, u, now)
	return changed(tag, err, "Unassigned promo code")
}

func (t *transaction) RecordPromoRequest(ctx context.Context, campaign string, u int64, now time.Time) error {
	_, err := t.tx.Exec(ctx,
		"INSERT INTO promo_requests(campaign_id,user_id,created_at) VALUES($1,$2,$3)", campaign, u, now,
	)
	return err
}

func (t *transaction) HasEntry(ctx context.Context, d string, u int64) (bool, error) {
	var yes bool
	err := t.tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM entries WHERE draw_id=$1 AND user_id=$2)", d, u).Scan(&yes)
	return yes, err
}

func (t *transaction) AddEntry(ctx context.Context, d string, u int64, now time.Time) error {
	_, err := t.tx.Exec(ctx, "INSERT INTO entries(draw_id,user_id,created_at) VALUES($1,$2,$3)", d, u, now)
	return err
}

func (t *transaction) Audit(ctx context.Context, c, action, detail string, now time.Time) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO audit_events(campaign_id, action, detail, created_at)
		VALUES($1, $2, $3, $4)`, c, action, detail, now)
	return err
}

func (t *transaction) PendingClaim(ctx context.Context, u int64) (string, string, error) {
	var offer, name string
	err := t.tx.QueryRow(ctx, "SELECT offer_id,name FROM pending_claims WHERE user_id=$1", u).Scan(&offer, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return offer, name, err
}

func (t *transaction) SetPendingClaim(ctx context.Context, u int64, offer, name string) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO pending_claims(user_id, offer_id, name)
		VALUES($1, $2, $3)
		ON CONFLICT(user_id) DO UPDATE
		SET offer_id=excluded.offer_id, name=excluded.name`, u, offer, name)
	return err
}

func (t *transaction) ClearPendingClaim(ctx context.Context, u int64) error {
	_, err := t.tx.Exec(ctx, "DELETE FROM pending_claims WHERE user_id=$1", u)
	return err
}
