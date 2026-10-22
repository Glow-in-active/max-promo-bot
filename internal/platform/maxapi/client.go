package maxapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
)

const defaultBaseURL = "https://platform-api2.max.ru"
const responseLimit = 1 << 20

type Config struct {
	Token      string
	BaseURL    string
	BannerURL  string
	CAFile     string
	HTTPClient *http.Client
}

type Client struct {
	token, baseURL, bannerURL string
	http                      *http.Client
	requests                  *pacer
	sends                     *recipientPacer
	answers                   *recipientPacer
}

func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.Token) == "" {
		return nil, configurationError("MAX token is required")
	}
	if config.BaseURL == "" {
		config.BaseURL = defaultBaseURL
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.User != nil || strings.Contains(config.BaseURL, "#") {
		return nil, configurationError("MAX base URL must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if config.BannerURL != "" {
		image, parseErr := url.Parse(config.BannerURL)
		if parseErr != nil || image.Scheme != "https" || image.Hostname() == "" || image.Opaque != "" || image.User != nil || strings.Contains(config.BannerURL, "#") {
			return nil, configurationError("MAX banner must be a public HTTPS image URL without credentials or fragment")
		}
	}
	httpClient := &http.Client{Timeout: 10 * time.Second}
	if config.HTTPClient != nil {
		clone := *config.HTTPClient
		httpClient = &clone
		if httpClient.Timeout <= 0 {
			httpClient.Timeout = 10 * time.Second
		}
	}
	if config.CAFile != "" {
		transport := httpClient.Transport
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
			roots, err = x509.SystemCertPool()
			if err != nil {
				return nil, fmt.Errorf("load system CA certificates: %w", err)
			}
		}
		pem, err := os.ReadFile(config.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read MAX CA file: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, configurationError("MAX_CA_FILE contains no certificates")
		}
		tlsConfig.RootCAs = roots
		tlsConfig.MinVersion = max(tlsConfig.MinVersion, tls.VersionTLS12)
		clone.TLSClientConfig = tlsConfig
		httpClient.Transport = clone
	}
	// Redirects could otherwise forward a bot token to another origin.
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	requests := newPacer(time.Second/30 + time.Nanosecond)
	sends, answers := newRecipientPacer(500*time.Millisecond), newRecipientPacer(500*time.Millisecond)
	sends.parent, answers.parent = requests, requests
	return &Client{token: config.Token, baseURL: strings.TrimRight(config.BaseURL, "/"), bannerURL: config.BannerURL, http: httpClient, requests: requests, sends: sends, answers: answers}, nil
}

type configurationError string

func (e configurationError) Error() string { return string(e) }

func (e configurationError) LogAttrs() []slog.Attr {
	return []slog.Attr{slog.String("error_code", "invalid_max_configuration"), slog.String("reason", string(e))}
}

// Error carries safe metadata only. The response body can contain personal data
// and is deliberately excluded from errors and logs.
type Error struct {
	Status     int
	RetryAfter time.Duration
	Ambiguous  bool
	cause      error
}

func (e *Error) HTTPStatus() int { return e.Status }

func (e *Error) Error() string {
	if e.Status == 0 {
		return "MAX request failed before a delivery acknowledgement"
	}
	return fmt.Sprintf("MAX returned HTTP %d", e.Status)
}
func (e *Error) Temporary() bool {
	return e.Status == 0 || e.Status == http.StatusTooManyRequests || e.Status >= 500
}
func (e *Error) Uncertain() bool           { return e.Ambiguous }
func (e *Error) Unwrap() error             { return e.cause }
func (e *Error) RetryDelay() time.Duration { return e.RetryAfter }

func (c *Client) Subscription(ctx context.Context, channelID, userID int64) (app.Subscription, error) {
	query := url.Values{"user_ids": {strconv.FormatInt(userID, 10)}}
	endpoint := fmt.Sprintf("/chats/%d/members?%s", channelID, query.Encode())
	var response struct {
		Members *[]struct {
			UserID int64 `json:"user_id"`
		} `json:"members"`
	}
	if err := c.request(ctx, http.MethodGet, endpoint, nil, &response, false, nil); err != nil {
		return app.SubscriptionUnknown, err
	}
	if response.Members == nil {
		return app.SubscriptionUnknown, errors.New("MAX membership response has no members field")
	}
	for _, member := range *response.Members {
		if member.UserID == userID {
			return app.SubscriptionYes, nil
		}
	}
	return app.SubscriptionNo, nil
}

type attachment struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}
type button struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Payload string `json:"payload,omitempty"`
	URL     string `json:"url,omitempty"`
}

func (c *Client) Send(ctx context.Context, message domain.Message) error {
	body := struct {
		Text        string       `json:"text"`
		Attachments []attachment `json:"attachments,omitempty"`
	}{Text: message.Text}
	if message.Banner && c.bannerURL != "" {
		body.Attachments = append(body.Attachments, attachment{Type: "image", Payload: map[string]string{"url": c.bannerURL}})
	}
	if len(message.Buttons) > 0 {
		rows := make([][]button, 0, len(message.Buttons))
		for _, item := range message.Buttons {
			b := button{Type: "callback", Text: item.Text, Payload: item.Payload}
			if item.URL != "" {
				b.Type = "link"
				b.Payload = ""
				b.URL = item.URL
			}
			rows = append(rows, []button{b})
		}
		body.Attachments = append(body.Attachments, attachment{Type: "inline_keyboard", Payload: map[string]any{"buttons": rows}})
	}
	var response struct {
		Message *struct {
			Body struct {
				MID string `json:"mid"`
			} `json:"body"`
		} `json:"message"`
	}
	if err := c.request(ctx, http.MethodPost, "/messages?user_id="+strconv.FormatInt(message.UserID, 10), body, &response, true, &recipientRequest{c.sends, message.UserID}); err != nil {
		return err
	}
	if response.Message == nil || strings.TrimSpace(response.Message.Body.MID) == "" {
		return &Error{Status: http.StatusOK, Ambiguous: true}
	}
	return nil
}

func (c *Client) AnswerCallback(ctx context.Context, userID int64, callbackID string) error {
	if strings.TrimSpace(callbackID) == "" {
		return errors.New("MAX callback ID is required")
	}
	query := url.Values{"callback_id": {callbackID}}
	var response struct {
		Success *bool `json:"success"`
	}
	if err := c.request(ctx, http.MethodPost, "/answers?"+query.Encode(), struct{}{}, &response, false, &recipientRequest{c.answers, userID}); err != nil {
		return err
	}
	if response.Success == nil || !*response.Success {
		return errors.New("MAX callback acknowledgement was rejected")
	}
	return nil
}

type recipientRequest struct {
	limiter *recipientPacer
	userID  int64
}

func (c *Client) request(ctx context.Context, method, endpoint string, body, output any, sending bool, recipient *recipientRequest) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode MAX request: %w", err)
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, input)
	if err != nil {
		return errors.New("construct MAX request")
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if recipient != nil {
		err = recipient.limiter.wait(ctx, recipient.userID)
	} else {
		err = c.requests.wait(ctx)
	}
	if err != nil {
		return err
	}
	response, err := c.http.Do(req)
	if err != nil {
		cause := ctx.Err()
		if cause == nil {
			var timeout interface{ Timeout() bool }
			switch {
			case errors.Is(err, context.Canceled):
				cause = context.Canceled
			case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
				cause = context.DeadlineExceeded
			}
		}
		return &Error{Ambiguous: sending, cause: cause}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return &Error{Status: response.StatusCode, RetryAfter: retryAfter(response.Header.Get("Retry-After")), Ambiguous: sending && response.StatusCode >= 500}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil || len(data) > responseLimit {
		return &Error{Status: response.StatusCode, Ambiguous: sending}
	}
	if err = json.Unmarshal(data, output); err != nil {
		return &Error{Status: response.StatusCode, Ambiguous: sending}
	}
	return nil
}

func retryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	const maxDuration = time.Duration(1<<63 - 1)
	if value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		seconds, err := strconv.ParseUint(value, 10, 64)
		// Saturate only at the duration representation limit to prevent overflow.
		if err != nil || seconds > uint64(maxDuration/time.Second) {
			return maxDuration
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait
		}
	}
	return 0
}
