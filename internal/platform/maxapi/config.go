package maxapi

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func validateURLs(config Config) error {
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.User != nil ||
		strings.Contains(config.BaseURL, "#") {
		return configurationError("MAX base URL must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if config.BannerURL != "" {
		image, err := url.Parse(config.BannerURL)
		if err != nil || image.Scheme != "https" || image.Hostname() == "" ||
			image.Opaque != "" || image.User != nil || strings.Contains(config.BannerURL, "#") {
			return configurationError("MAX banner must be a public HTTPS image URL without credentials or fragment")
		}
	}
	return nil
}

func newHTTPClient(config Config) (*http.Client, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	if config.HTTPClient != nil {
		clone := *config.HTTPClient
		client = &clone
		if client.Timeout <= 0 {
			client.Timeout = 10 * time.Second
		}
	}
	if config.CAFile != "" {
		transport, err := transportWithCA(client.Transport, config.CAFile)
		if err != nil {
			return nil, err
		}
		client.Transport = transport
	}

	// Redirects could otherwise forward a bot token to another origin.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client, nil
}

func transportWithCA(transport http.RoundTripper, path string) (*http.Transport, error) {
	if transport == nil {
		transport = http.DefaultTransport
	}
	base, ok := transport.(*http.Transport)
	if !ok {
		return nil, configurationError("MAX CA file requires an HTTP transport with configurable TLS")
	}
	clone := base.Clone()
	tlsConfig := clone.TLSClientConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	}

	var roots *x509.CertPool
	if tlsConfig.RootCAs != nil {
		roots = tlsConfig.RootCAs.Clone()
	} else {
		var err error
		roots, err = x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("load system CA certificates: %w", err)
		}
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read MAX CA file: %w", err)
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, configurationError("MAX_CA_FILE contains no certificates")
	}

	tlsConfig.RootCAs = roots
	tlsConfig.MinVersion = max(tlsConfig.MinVersion, tls.VersionTLS12)
	clone.TLSClientConfig = tlsConfig
	return clone, nil
}
