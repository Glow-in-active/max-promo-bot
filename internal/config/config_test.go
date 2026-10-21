package config

import (
	"log/slog"
	"strings"
	"testing"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	clearDatabaseEnv(t)
	for _, name := range []string{"HTTP_ADDR", "MODE", "ADMIN_TOKEN", "DEMO_TOKEN", "MAX_WEBHOOK_SECRET", "CAMPAIGN_ID", "MAX_TOKEN", "MAX_API_URL", "MAX_BANNER_URL", "MAX_CA_FILE", "LOG_LEVEL"} {
		t.Setenv(name, "")
	}
}

func TestLoadUsesDefaultsWithoutServingCredentials(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address != ":8080" || cfg.Mode != "production" || cfg.MAXURL != "https://platform-api2.max.ru" || cfg.LogLevel != "info" {
		t.Fatal("configuration defaults changed")
	}
	if cfg.AdminToken != "" || cfg.DemoToken != "" || cfg.WebhookSecret != "" || cfg.CampaignID != "" || cfg.MAXToken != "" || cfg.BannerURL != "" || cfg.CAFile != "" {
		t.Fatal("missing credentials or optional settings received a default")
	}
	if cfg.Validate() == nil {
		t.Fatal("serving without credentials accepted")
	}
}

func TestLoadUsesConfiguredFields(t *testing.T) {
	clearConfigEnv(t)
	for name, value := range map[string]string{
		"DATABASE_URL":       "postgres://db.example.org/promo?sslmode=require",
		"HTTP_ADDR":          "127.0.0.1:8181",
		"MODE":               "demo",
		"ADMIN_TOKEN":        strings.Repeat("a", 24),
		"DEMO_TOKEN":         strings.Repeat("d", 16),
		"MAX_WEBHOOK_SECRET": "webhook-secret-value",
		"CAMPAIGN_ID":        "campaign-test",
		"MAX_TOKEN":          "max-token-value",
		"MAX_API_URL":        "https://max.example.org",
		"MAX_BANNER_URL":     "https://example.org/banner.png",
		"MAX_CA_FILE":        "/run/secrets/max-ca.pem",
		"LOG_LEVEL":          " WARN ",
	} {
		t.Setenv(name, value)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		DatabaseURL:   "postgres://db.example.org/promo?sslmode=require",
		Address:       "127.0.0.1:8181",
		Mode:          "demo",
		AdminToken:    strings.Repeat("a", 24),
		DemoToken:     strings.Repeat("d", 16),
		WebhookSecret: "webhook-secret-value",
		CampaignID:    "campaign-test",
		MAXToken:      "max-token-value",
		MAXURL:        "https://max.example.org",
		BannerURL:     "https://example.org/banner.png",
		CAFile:        "/run/secrets/max-ca.pem",
		LogLevel:      "warn",
	}
	if cfg != want {
		t.Fatal("a configured field was ignored or changed")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadNormalizesLogLevels(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("LOG_LEVEL", " "+strings.ToUpper(level)+" ")
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LogLevel != level {
				t.Fatal("log level was not normalized")
			}
		})
	}
}

func TestLoadRejectsInvalidLogLevelWithSafeDiagnostic(t *testing.T) {
	clearConfigEnv(t)
	const unsafe = "https://user:never-log-this-password@example.org"
	t.Setenv("LOG_LEVEL", unsafe)
	_, err := Load()
	if err == nil {
		t.Fatal("invalid log level accepted")
	}
	if err.Error() != "LOG_LEVEL must be debug, info, warn, or error" {
		t.Fatal("log level error includes an untrusted value")
	}
	diagnostic, ok := err.(interface{ LogAttrs() []slog.Attr })
	if !ok {
		t.Fatal("configuration error has no safe diagnostic")
	}
	attrs := diagnostic.LogAttrs()
	if len(attrs) != 2 || attrs[0].Key != "error_code" || attrs[0].Value.String() != "invalid_configuration" || attrs[1].Key != "reason" || attrs[1].Value.String() != err.Error() {
		t.Fatal("configuration diagnostic does not identify the safe failure reason")
	}
}

func TestConfigRejectsUnsafeModesAndSecrets(t *testing.T) {
	base := Config{DatabaseURL: "postgres://db/promo", Address: ":8080", Mode: "demo", AdminToken: strings.Repeat("a", 24), DemoToken: strings.Repeat("d", 16), WebhookSecret: strings.Repeat("w", 16)}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){func(c *Config) { c.DatabaseURL = "" }, func(c *Config) { c.Mode = "unknown" }, func(c *Config) { c.AdminToken = "short" }, func(c *Config) { c.DemoToken = "short" }} {
		c := base
		change(&c)
		if err := c.Validate(); err == nil {
			t.Fatal("accepted invalid configuration")
		}
	}
	prod := base
	prod.Mode = "production"
	if prod.Validate() == nil {
		t.Fatal("production without MAX accepted")
	}
	prod.MAXToken = "token"
	prod.WebhookSecret = strings.Repeat("w", 16)
	prod.CampaignID = "campaign"
	prod.MAXURL = "https://platform-api2.max.ru"
	prod.BannerURL = "https://example.org/banner.png"
	if err := prod.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.AdminToken = "local-administrator-token" },
		func(c *Config) { c.WebhookSecret = "local-webhook-secret" },
		func(c *Config) { c.AdminToken = "localdemo-administrator-token" },
		func(c *Config) { c.WebhookSecret = "localdemo-webhook-secret" },
		func(c *Config) { c.MAXURL = "http://example.org" },
		func(c *Config) { c.BannerURL = "https://user:password@example.org/image.png" },
		func(c *Config) { c.AdminToken = strings.Repeat(" ", 24) },
		func(c *Config) { c.AdminToken = strings.Repeat("a", 24) + "\n" },
		func(c *Config) { c.WebhookSecret = strings.Repeat(" ", 16) },
		func(c *Config) { c.WebhookSecret = "\t" + strings.Repeat("w", 16) },
		func(c *Config) { c.MAXToken = "token\r\nvalue" },
		func(c *Config) { c.MAXToken = " " },
		func(c *Config) { c.MAXURL = "https://platform-api2.max.ru#section" },
		func(c *Config) { c.MAXURL = "https://platform-api2.max.ru#" },
		func(c *Config) { c.MAXURL = "https://platform-api2.max.ru?" },
		func(c *Config) { c.MAXURL = "https://platform-api2.max.ru?x=y" },
		func(c *Config) { c.BannerURL = "https://example.org/banner.png#section" },
	} {
		c := prod
		change(&c)
		if c.Validate() == nil {
			t.Fatal("unsafe production configuration accepted")
		}
	}
}

func TestDemoDefaultsAreResolvedByConfiguration(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("MODE", "demo")
	value, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if value.WebhookSecret != "local-webhook-secret" {
		t.Fatal("demo webhook default was not resolved")
	}
	t.Setenv("MODE", "production")
	value, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if value.WebhookSecret != "" {
		t.Fatal("production received a demo webhook secret")
	}
}

func TestSimulatorRequiresExplicitAPIAndWebhookConfiguration(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("MODE", "simulator")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MAXURL != "" || cfg.WebhookSecret != "" {
		t.Fatal("simulator API or webhook configuration was inferred")
	}
	t.Setenv("MAX_API_URL", "http://max-emulator:3000")
	t.Setenv("MAX_WEBHOOK_SECRET", "local-webhook-secret")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MAXURL != "http://max-emulator:3000" || cfg.WebhookSecret != "local-webhook-secret" {
		t.Fatal("simulator API or webhook configuration was ignored")
	}
}

func TestSimulatorConfiguration(t *testing.T) {
	base := Config{
		DatabaseURL: "postgres://db/promo", Address: ":8080", Mode: "simulator",
		AdminToken: "local-administrator-token", DemoToken: "local-simulator-token",
		WebhookSecret: "local-webhook-secret", MAXToken: "local-max-token",
		MAXURL: "http://max-emulator:3000",
	}
	for _, address := range []string{"http://max-emulator:3000", "http://127.0.0.1:3000", "http://[::1]:3000", "https://max.example.org"} {
		t.Run(address, func(t *testing.T) {
			cfg := base
			cfg.MAXURL = address
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, test := range []struct {
		name   string
		change func(*Config)
	}{
		{"missing API", func(c *Config) { c.MAXURL = "" }},
		{"relative API", func(c *Config) { c.MAXURL = "/max" }},
		{"API scheme", func(c *Config) { c.MAXURL = "ftp://max-emulator:3000" }},
		{"API credentials", func(c *Config) { c.MAXURL = "http://user:secret@max-emulator:3000" }},
		{"API query", func(c *Config) { c.MAXURL = "http://max-emulator:3000?token=secret" }},
		{"empty API query", func(c *Config) { c.MAXURL = "http://max-emulator:3000?" }},
		{"API fragment", func(c *Config) { c.MAXURL = "http://max-emulator:3000#secret" }},
		{"empty API fragment", func(c *Config) { c.MAXURL = "http://max-emulator:3000#" }},
		{"missing token", func(c *Config) { c.MAXToken = "" }},
		{"invalid token", func(c *Config) { c.MAXToken = "local token" }},
		{"missing demo token", func(c *Config) { c.DemoToken = "" }},
		{"short demo token", func(c *Config) { c.DemoToken = "short" }},
		{"missing admin token", func(c *Config) { c.AdminToken = "" }},
		{"missing webhook secret", func(c *Config) { c.WebhookSecret = "" }},
		{"insecure banner", func(c *Config) { c.BannerURL = "http://max-emulator:3000/banner.png" }},
		{"banner credentials", func(c *Config) { c.BannerURL = "https://user:secret@example.org/banner.png" }},
		{"banner fragment", func(c *Config) { c.BannerURL = "https://example.org/banner.png#fragment" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			test.change(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("invalid simulator configuration accepted")
			}
		})
	}
	base.BannerURL = "https://example.org/banner.png?image=1"
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookSecretUsesTheMAXAlphabetAndLength(t *testing.T) {
	for _, mode := range []string{"demo", "simulator", "production"} {
		t.Run(mode, func(t *testing.T) {
			base := Config{
				DatabaseURL: "postgres://db/promo", Address: ":8080", Mode: mode,
				AdminToken: strings.Repeat("a", 24), DemoToken: strings.Repeat("d", 16),
				MAXToken: "max-token", MAXURL: "https://max.example.org", CampaignID: "campaign",
				BannerURL: "https://example.org/banner.png",
			}
			for _, secret := range []string{"Abc_0123456789-xy", strings.Repeat("s", 256)} {
				cfg := base
				cfg.WebhookSecret = secret
				if err := cfg.Validate(); err != nil {
					t.Fatalf("valid webhook secret rejected: %v", err)
				}
			}
			for _, test := range []struct{ name, secret string }{
				{"empty", ""},
				{"short", strings.Repeat("s", 15)},
				{"long", strings.Repeat("s", 257)},
				{"plus", strings.Repeat("s", 16) + "+"},
				{"slash", strings.Repeat("s", 16) + "/"},
				{"equals", strings.Repeat("s", 16) + "="},
				{"unicode", strings.Repeat("s", 16) + "я"},
				{"newline", strings.Repeat("s", 16) + "\n"},
			} {
				t.Run(test.name, func(t *testing.T) {
					cfg := base
					cfg.WebhookSecret = test.secret
					if err := cfg.Validate(); err == nil {
						t.Fatal("invalid webhook secret accepted")
					}
				})
			}
		})
	}
}
