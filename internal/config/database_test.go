package config

import (
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func clearDatabaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LOG_LEVEL", "info")
	for _, name := range []string{"DATABASE_URL", "POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_DB", "POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_SSLMODE"} {
		t.Setenv(name, "")
	}
}

func TestDatabaseComponentsUseNativeDefaults(t *testing.T) {
	clearDatabaseEnv(t)
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pgx.ParseConfig(config.DatabaseURL)
	if err != nil {
		t.Fatal("generated database URL is invalid")
	}
	if parsed.Host != "localhost" || parsed.Port != 5432 || parsed.Database != "promo" || parsed.User != "promo" || parsed.Password != "" {
		t.Fatal("native database defaults changed")
	}
	uri, err := url.Parse(config.DatabaseURL)
	if err != nil {
		t.Fatal("generated database URL is invalid")
	}
	if uri.Query().Get("sslmode") != "disable" {
		t.Fatal("native SSL mode default changed")
	}
}

func TestDatabaseComponentsPreserveCustomFieldsAndEscapedCredentials(t *testing.T) {
	clearDatabaseEnv(t)
	const user = "service@ops"
	const password = "p@ss:/?#%+ space'漢"
	const database = "campaign/2026?prod#"
	values := map[string]string{"POSTGRES_HOST": "db.internal", "POSTGRES_PORT": "6543", "POSTGRES_DB": database, "POSTGRES_USER": user, "POSTGRES_PASSWORD": password, "POSTGRES_SSLMODE": "require"}
	for key, value := range values {
		t.Setenv(key, value)
	}
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pgx.ParseConfig(config.DatabaseURL)
	if err != nil {
		t.Fatal("generated database URL is invalid")
	}
	if parsed.Host != "db.internal" || parsed.Port != 6543 || parsed.Database != database || parsed.User != user || parsed.Password != password {
		t.Fatal("database fields or escaped credentials changed")
	}
	uri, err := url.Parse(config.DatabaseURL)
	if err != nil {
		t.Fatal("generated database URL is invalid")
	}
	if uri.Query().Get("sslmode") != "require" {
		t.Fatal("custom SSL mode was not used")
	}
	if strings.Contains(config.DatabaseURL, password) {
		t.Fatal("password was interpolated without URL encoding")
	}
}

func TestDatabaseComponentsAcceptIPv6(t *testing.T) {
	for _, host := range []string{"::1", "[2001:db8::1]", "fe80::1%en0"} {
		t.Run(host, func(t *testing.T) {
			clearDatabaseEnv(t)
			t.Setenv("POSTGRES_HOST", host)
			t.Setenv("POSTGRES_PORT", "6432")
			config, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := pgx.ParseConfig(config.DatabaseURL)
			if err != nil {
				t.Fatal("generated IPv6 database URL is invalid")
			}
			if parsed.Host != strings.Trim(host, "[]") || parsed.Port != 6432 {
				t.Fatal("IPv6 host or port changed")
			}
		})
	}
}

func TestDatabaseURLOverridesAllComponentsWithoutRewriting(t *testing.T) {
	for name, explicit := range map[string]string{
		"URL": "postgresql://explicit:escaped%40password@db.example:5444/custom?sslmode=verify-full&application_name=override",
		"DSN": "host=db.example port=5444 dbname=custom user=explicit password='unchanged password' sslmode=verify-full application_name=override",
	} {
		t.Run(name, func(t *testing.T) {
			clearDatabaseEnv(t)
			t.Setenv("DATABASE_URL", explicit)
			t.Setenv("POSTGRES_HOST", "invalid:host")
			t.Setenv("POSTGRES_PORT", "invalid")
			t.Setenv("POSTGRES_DB", "ignored")
			t.Setenv("POSTGRES_USER", "ignored")
			t.Setenv("POSTGRES_PASSWORD", "ignored")
			t.Setenv("POSTGRES_SSLMODE", "invalid")
			config, err := Load()
			if err != nil {
				t.Fatal("explicit DATABASE_URL must take precedence")
			}
			if config.DatabaseURL != explicit {
				t.Fatal("explicit DATABASE_URL was rewritten")
			}
		})
	}
}

func TestDatabaseComponentsRejectInvalidValuesWithoutSecrets(t *testing.T) {
	const password = "never-include-this-password"
	cases := []struct{ name, key, value string }{
		{"zero port", "POSTGRES_PORT", "0"}, {"negative port", "POSTGRES_PORT", "-1"}, {"large port", "POSTGRES_PORT", "65536"}, {"text port", "POSTGRES_PORT", "postgres"}, {"fractional port", "POSTGRES_PORT", "5432.1"},
		{"host contains port", "POSTGRES_HOST", "db.example:6432"}, {"host contains URL path", "POSTGRES_HOST", "db.example/secret"}, {"empty host", "POSTGRES_HOST", " "}, {"unsupported SSL mode", "POSTGRES_SSLMODE", "unsafe"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			clearDatabaseEnv(t)
			t.Setenv("POSTGRES_PASSWORD", password)
			t.Setenv(test.key, test.value)
			_, err := Load()
			if err == nil {
				t.Fatal("invalid database component accepted")
			}
			if !strings.Contains(err.Error(), test.key) {
				t.Fatalf("error does not identify the invalid setting: %v", err)
			}
			if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "postgres://") {
				t.Fatal("database error exposed credentials or a connection URL")
			}
		})
	}
}
