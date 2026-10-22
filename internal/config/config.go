package config

import (
	"net/url"
	"os"
	"strings"
	"unicode"
)

type Config struct {
	DatabaseURL   string
	Address       string
	Mode          string
	AdminToken    string
	DemoToken     string
	WebhookSecret string
	CampaignID    string
	MAXToken      string
	MAXURL        string
	BannerURL     string
	CAFile        string
	LogLevel      string
}

func Load() (Config, error) {
	address, err := HTTPAddress()
	if err != nil {
		return Config{}, err
	}
	databaseURL, err := databaseURLFromEnv()
	if err != nil {
		return Config{}, err
	}
	logLevel := strings.ToLower(strings.TrimSpace(env("LOG_LEVEL", "info")))
	mode := env("MODE", "production")
	maxURL := "https://platform-api2.max.ru"
	if mode == "simulator" {
		maxURL = ""
	}
	webhookSecret := os.Getenv("MAX_WEBHOOK_SECRET")
	if mode == "demo" && webhookSecret == "" {
		webhookSecret = "local-webhook-secret"
	}
	switch logLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, validationError("LOG_LEVEL must be debug, info, warn, or error")
	}
	return Config{
		DatabaseURL:   databaseURL,
		Address:       address,
		Mode:          mode,
		AdminToken:    os.Getenv("ADMIN_TOKEN"),
		DemoToken:     os.Getenv("DEMO_TOKEN"),
		WebhookSecret: webhookSecret,
		CampaignID:    os.Getenv("CAMPAIGN_ID"),
		MAXToken:      os.Getenv("MAX_TOKEN"),
		MAXURL:        env("MAX_API_URL", maxURL),
		BannerURL:     os.Getenv("MAX_BANNER_URL"),
		CAFile:        os.Getenv("MAX_CA_FILE"),
		LogLevel:      logLevel,
	}, nil
}

func (c Config) Validate() error {
	if err := validateHTTPAddress(c.Address); err != nil {
		return err
	}
	if c.DatabaseURL == "" {
		return validationError("database connection is not configured")
	}
	if c.Mode != "demo" && c.Mode != "simulator" && c.Mode != "production" {
		return validationError("MODE must be demo, simulator, or production")
	}
	if !validToken(c.AdminToken, 24) {
		return validationError("ADMIN_TOKEN must contain at least 24 characters without whitespace or control characters")
	}
	if !validWebhookSecret(c.WebhookSecret) {
		return validationError("MAX_WEBHOOK_SECRET must contain 16 to 256 ASCII letters, digits, underscores or hyphens")
	}
	if c.Mode == "demo" || c.Mode == "simulator" {
		if !validToken(c.DemoToken, 16) {
			return validationError("DEMO_TOKEN must contain at least 16 characters without whitespace or control characters")
		}
	}
	if c.Mode == "demo" {
		return nil
	}
	if c.Mode == "simulator" {
		if !validToken(c.MAXToken, 1) {
			return validationError("MAX_TOKEN must not be empty or contain whitespace or control characters")
		}
		if !validSimulatorURL(c.MAXURL) {
			return validationError("simulator MAX_API_URL must be an absolute HTTP(S) URL without credentials, query or fragment")
		}
		if c.BannerURL != "" && !validImageURL(c.BannerURL) {
			return validationError("MAX_BANNER_URL must use HTTPS without embedded credentials or fragments")
		}
		return nil
	}
	if isLocalToken(c.AdminToken) {
		return validationError("ADMIN_TOKEN is reserved for local use")
	}
	if isLocalToken(c.WebhookSecret) {
		return validationError("MAX_WEBHOOK_SECRET is reserved for local use")
	}
	if !validToken(c.MAXToken, 1) || strings.TrimSpace(c.CampaignID) == "" || c.BannerURL == "" {
		return validationError("production requires MAX_TOKEN, MAX_WEBHOOK_SECRET (16+ characters), CAMPAIGN_ID and MAX_BANNER_URL")
	}
	for _, raw := range []string{c.MAXURL, c.BannerURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "#") {
			return validationError("production MAX URLs must use HTTPS without embedded credentials or fragments")
		}
		if raw == c.MAXURL && (u.RawQuery != "" || u.ForceQuery) {
			return validationError("MAX_API_URL must not contain a query")
		}
	}
	return nil
}

func validSimulatorURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil &&
		(u.Scheme == "https" || u.Scheme == "http") &&
		u.Hostname() != "" && u.Opaque == "" && u.User == nil &&
		!strings.Contains(raw, "#") && u.RawQuery == "" && !u.ForceQuery
}

func validImageURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.Opaque == "" && u.User == nil && !strings.Contains(raw, "#")
}

func validToken(value string, minimum int) bool {
	return len(value) >= minimum && strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func validWebhookSecret(value string) bool {
	if len(value) < 16 || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func isLocalToken(token string) bool {
	return strings.HasPrefix(token, "local-") || strings.HasPrefix(token, "localdemo-")
}
