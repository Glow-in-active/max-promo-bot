//go:build integration

package postgres

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestDatabaseURLPoolLimitIsRespected(t *testing.T) {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Fatal("integration tests require TEST_DATABASE_URL")
	}
	parsed, err := url.Parse(raw)
	requireNoError(t, err)
	query := parsed.Query()
	query.Set("pool_max_conns", "2")
	parsed.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := New(ctx, parsed.String())
	requireNoError(t, err)
	defer store.Close()
	first, err := store.pool.Acquire(ctx)
	requireNoError(t, err)
	defer first.Release()
	second, err := store.pool.Acquire(ctx)
	requireNoError(t, err)
	defer second.Release()
	waitCtx, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	third, err := store.pool.Acquire(waitCtx)
	if third != nil {
		third.Release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pool exceeded the configured connection limit: %v", err)
	}
	first.Release()
	recovered, err := store.pool.Acquire(ctx)
	requireNoError(t, err)
	recovered.Release()
}
