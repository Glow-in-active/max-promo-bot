//go:build integration

package testdb

import (
	"context"
	"crypto/rand"
	"math/big"

	"github.com/jackc/pgx/v5"
)

func IsolateDemoUsers(ctx context.Context, conn *pgx.Conn, schema string) error {
	base, err := rand.Int(rand.Reader, big.NewInt(1<<48))
	if err != nil {
		return err
	}
	sequence := pgx.Identifier{schema, "demo_user_ids"}.Sanitize()
	_, err = conn.Exec(ctx, "SELECT setval($1::regclass, $2, false)", sequence, 1_000_000_000_000+base.Int64())
	return err
}
