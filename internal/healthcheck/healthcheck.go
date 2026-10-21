package healthcheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func Check(ctx context.Context, address string) error {
	endpoint, err := readinessURL(address)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return &probeError{cause: err}
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return &probeError{cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &probeError{status: response.StatusCode}
	}
	return nil
}

type probeError struct {
	status int
	cause  error
}

func (e *probeError) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("readiness returned HTTP %d", e.status)
	}
	return "readiness request failed"
}

func (e *probeError) Unwrap() error { return e.cause }

func (e *probeError) LogAttrs() []slog.Attr {
	return []slog.Attr{slog.String("error_code", "readiness_failed"), slog.String("reason", e.Error())}
}

func readinessURL(address string) (string, error) {
	invalid := errors.New("readiness address must use host:port with a port from 1 to 65535")
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" || strings.ContainsAny(host, " \t\r\n/@?#\\[]") {
		return "", invalid
	}
	for _, digit := range port {
		if digit < '0' || digit > '9' {
			return "", invalid
		}
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return "", invalid
	}
	if host == "" {
		host = "127.0.0.1"
	} else if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.IsUnspecified() {
			host = "::1"
			if ip.Is4() {
				host = "127.0.0.1"
			}
		}
	} else if strings.ContainsAny(host, ":%") {
		return "", invalid
	}
	endpoint := url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.FormatUint(number, 10)), Path: "/readyz"}
	return endpoint.String(), nil
}
