//go:build integration

package config

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDatabaseComponentsConnectToConfiguredPostgres(t *testing.T) {
	explicit := os.Getenv("TEST_DATABASE_URL")
	if explicit == "" {
		t.Fatal("integration tests require TEST_DATABASE_URL")
	}
	target, err := pgx.ParseConfig(explicit)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	uri, err := url.Parse(explicit)
	if err != nil {
		t.Fatal("invalid test database URL")
	}
	sslmode := uri.Query().Get("sslmode")
	if sslmode == "" {
		sslmode = "prefer"
	}
	clearDatabaseEnv(t)
	for key, value := range map[string]string{"POSTGRES_HOST": target.Host, "POSTGRES_PORT": strconv.Itoa(int(target.Port)), "POSTGRES_DB": target.Database, "POSTGRES_USER": target.User, "POSTGRES_PASSWORD": target.Password, "POSTGRES_SSLMODE": sslmode} {
		t.Setenv(key, value)
	}
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	connectionConfig, err := pgx.ParseConfig(config.DatabaseURL)
	if err != nil {
		t.Fatal("component database configuration is invalid")
	}
	if connectionConfig.Host != target.Host || connectionConfig.Port != target.Port {
		t.Fatal("database host or port was ignored")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, connectionConfig)
	if err != nil {
		t.Fatal("could not connect with component database configuration")
	}
	defer conn.Close(ctx)
	var database, user string
	if err = conn.QueryRow(ctx, "SELECT current_database(),current_user").Scan(&database, &user); err != nil {
		t.Fatal("could not inspect the connected database")
	}
	if database != target.Database || user != target.User {
		t.Fatal("connected to a different database or role")
	}
}
