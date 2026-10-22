package config

import (
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func databaseURLFromEnv() (string, error) {
	if explicit := os.Getenv("DATABASE_URL"); explicit != "" {
		return explicit, nil
	}
	host := strings.TrimSpace(env("POSTGRES_HOST", "localhost"))
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if host == "" || strings.ContainsAny(host, "/?#@[] \t\r\n") {
		return "", validationError("POSTGRES_HOST must be a hostname or IP address")
	}
	if strings.Contains(host, ":") {
		if _, err := netip.ParseAddr(host); err != nil {
			return "", validationError("POSTGRES_HOST must be a hostname or IP address without a port")
		}
	}
	port := strings.TrimSpace(env("POSTGRES_PORT", "5432"))
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", validationError("POSTGRES_PORT must be an integer between 1 and 65535")
	}
	sslmode := env("POSTGRES_SSLMODE", "disable")
	switch sslmode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return "", validationError("POSTGRES_SSLMODE must be disable, allow, prefer, require, verify-ca or verify-full")
	}
	database := env("POSTGRES_DB", "promo")
	connection := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(env("POSTGRES_USER", "promo"), os.Getenv("POSTGRES_PASSWORD")),
		Host:     net.JoinHostPort(host, strconv.Itoa(number)),
		Path:     "/" + database,
		RawPath:  "/" + url.PathEscape(database),
		RawQuery: url.Values{"sslmode": {sslmode}}.Encode(),
	}
	return connection.String(), nil
}
