package maxapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(Config{Token: "private-token", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func TestSubscriptionContract(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		want       app.Subscription
		failure    bool
	}{
		{"member", `{"members":[{"user_id":42}]}`, 200, app.SubscriptionYes, false},
		{"absent", `{"members":[]}`, 200, app.SubscriptionNo, false},
		{"different member", `{"members":[{"user_id":43}]}`, 200, app.SubscriptionNo, false},
		{"permission failure", `{"message":"private data"}`, 403, app.SubscriptionUnknown, true},
		{"missing channel", `{}`, 404, app.SubscriptionUnknown, true},
		{"malformed", `{`, 200, app.SubscriptionUnknown, true},
		{"missing members", `{}`, 200, app.SubscriptionUnknown, true},
		{"null members", `{"members":null}`, 200, app.SubscriptionUnknown, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chats/-7/members" || r.URL.Query().Get("user_ids") != "42" || r.Header.Get("Authorization") != "private-token" {
					t.Errorf("invalid MAX contract: %s", r.URL)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			})
			got, err := client.Subscription(context.Background(), -7, 42)
			if got != test.want || (err != nil) != test.failure {
				t.Fatalf("got %v,%v", got, err)
			}
			if err != nil && strings.Contains(err.Error(), "private data") {
				t.Fatal("upstream body leaked")
			}
		})
	}
}
func TestSendContract(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/messages" || r.URL.Query().Get("user_id") != "42" || r.Header.Get("Authorization") != "private-token" {
			t.Errorf("invalid MAX send request")
		}
		var body struct {
			Text        string `json:"text"`
			Attachments []struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			} `json:"attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Text != "Hello" || len(body.Attachments) != 2 || body.Attachments[0].Type != "image" || body.Attachments[1].Type != "inline_keyboard" {
			t.Errorf("unexpected body: %+v", body)
		}
		var keyboard struct {
			Buttons [][]button `json:"buttons"`
		}
		_ = json.Unmarshal(body.Attachments[1].Payload, &keyboard)
		if len(keyboard.Buttons) != 2 || keyboard.Buttons[0][0].Payload != "promo" || keyboard.Buttons[1][0].URL != "https://example.org" {
			t.Errorf("unexpected keyboard: %+v", keyboard)
		}
		_, _ = w.Write([]byte(`{"message":{"body":{"mid":"sent-1"}}}`))
	})
	client.bannerURL = "https://example.org/banner.png"
	if err := client.Send(context.Background(), domain.Message{UserID: 42, Text: "Hello", Banner: true, Buttons: []domain.Button{{Text: "Code", Payload: "promo"}, {Text: "Partner", URL: "https://example.org"}}}); err != nil {
		t.Fatal(err)
	}
}
func TestSendUncertainResult(t *testing.T) {
	for _, test := range []struct {
		status               int
		body                 string
		uncertain, temporary bool
	}{
		{200, `{}`, true, false},
		{200, `{`, true, false},
		{200, `{"message":false}`, true, false},
		{200, `{"message":"failure"}`, true, false},
		{200, `{"message":{}}`, true, false},
		{200, `{"message":null}`, true, false},
		{200, `{"message":{"body":null}}`, true, false},
		{200, `{"message":{"body":{"mid":null}}}`, true, false},
		{200, `{"message":{"body":{"mid":""}}}`, true, false},
		{200, `{"message":{"body":{"mid":" \t\n"}}}`, true, false},
		{503, `{"token":"secret"}`, true, true},
		{429, `{}`, false, true},
		{403, `{}`, false, false},
	} {
		client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "999")
			w.WriteHeader(test.status)
			_, _ = w.Write([]byte(test.body))
		})
		err := client.Send(context.Background(), domain.Message{UserID: 42, Text: "test"})
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Uncertain() != test.uncertain || apiErr.Temporary() != test.temporary {
			t.Fatalf("unexpected error: %+v", err)
		}
		if apiErr.Status == 429 && apiErr.RetryDelay() != 999*time.Second {
			t.Fatal("provider retry delay was shortened")
		}
	}
}
func TestSendRespectsCancellationWhilePacing(t *testing.T) {
	calls := 0
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"message":{"body":{"mid":"sent-1"}}}`))
	})
	if err := client.Send(context.Background(), domain.Message{UserID: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := client.Send(ctx, domain.Message{UserID: 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if calls != 1 {
		t.Fatalf("canceled request reached MAX: %d", calls)
	}
}

func TestHTTPClientTimeoutPreservesOnlySafeCause(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	client, err := New(Config{Token: "private-token", BaseURL: server.URL + "/private-path", HTTPClient: &http.Client{Timeout: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Send(context.Background(), domain.Message{UserID: 42, Text: "Private message"})
	var apiErr *Error
	if !errors.As(err, &apiErr) || !apiErr.Uncertain() || !errors.Is(err, context.DeadlineExceeded) || apiErr.Unwrap() != context.DeadlineExceeded {
		t.Fatalf("HTTP client timeout lost its safe cause: %v", err)
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), server.URL) {
		t.Fatal("timeout exposed request data")
	}
}

func TestRedirectDoesNotLeakToken(t *testing.T) {
	reached := false
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer destination.Close()
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, http.StatusFound) })
	if err := client.Send(context.Background(), domain.Message{UserID: 42}); err == nil {
		t.Fatal("redirect accepted")
	}
	if reached {
		t.Fatal("followed redirect with token")
	}
}
func TestConfigValidation(t *testing.T) {
	for name, config := range map[string]Config{
		"missing token":         {},
		"relative base":         {Token: "token", BaseURL: "broken"},
		"base credentials":      {Token: "token", BaseURL: "https://user:secret@example.org"},
		"base query":            {Token: "token", BaseURL: "https://example.org?version=2"},
		"base empty query":      {Token: "token", BaseURL: "https://example.org?"},
		"base fragment":         {Token: "token", BaseURL: "https://example.org#section"},
		"base empty fragment":   {Token: "token", BaseURL: "https://example.org#"},
		"opaque base":           {Token: "token", BaseURL: "https:example.org"},
		"base missing host":     {Token: "token", BaseURL: "https://:443"},
		"insecure banner":       {Token: "token", BannerURL: "http://example.org/banner.png"},
		"banner credentials":    {Token: "token", BannerURL: "https://user:secret@example.org/banner.png"},
		"banner fragment":       {Token: "token", BannerURL: "https://example.org/banner.png#section"},
		"banner empty fragment": {Token: "token", BannerURL: "https://example.org/banner.png#"},
		"opaque banner":         {Token: "token", BannerURL: "https:example.org/banner.png"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(config); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	client, err := New(Config{Token: "token"})
	if err != nil || client.baseURL != defaultBaseURL {
		t.Fatalf("default config: %v", err)
	}
}

func TestBasePathPrefixAndSignedBannerURLArePreserved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot/v2/messages" || r.URL.Query().Get("user_id") != "42" {
			t.Errorf("base path prefix or recipient lost: %s", r.URL)
		}
		var body struct {
			Attachments []struct {
				Payload struct {
					URL string `json:"url"`
				} `json:"payload"`
			} `json:"attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		} else if len(body.Attachments) != 1 || body.Attachments[0].Payload.URL != "https://cdn.example.org/banner.png?signature=abc%2B123" {
			t.Errorf("signed banner URL changed: %+v", body)
		}
		_, _ = w.Write([]byte(`{"message":{"body":{"mid":"sent-1"}},"future_field":true}`))
	}))
	defer server.Close()
	client, err := New(Config{Token: "token", BaseURL: server.URL + "/bot/v2/", BannerURL: "https://cdn.example.org/banner.png?signature=abc%2B123"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Send(context.Background(), domain.Message{UserID: 42, Text: "Welcome", Banner: true}); err != nil {
		t.Fatal(err)
	}
}

func TestAdditionalCAUsesPrivateTransport(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"body":{"mid":"tls-delivery"}}}`))
	}))
	defer server.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, minimum := range []uint16{0, tls.VersionTLS10, tls.VersionTLS13} {
		t.Run(tls.VersionName(minimum), func(t *testing.T) {
			config := Config{Token: "token", BaseURL: server.URL, CAFile: caFile}
			var original *http.Transport
			if minimum != 0 {
				original = http.DefaultTransport.(*http.Transport).Clone()
				original.TLSClientConfig = &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: minimum}
				config.HTTPClient = &http.Client{Transport: original, Timeout: 3 * time.Second}
			}
			client, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			defer client.http.CloseIdleConnections()
			transport := client.http.Transport.(*http.Transport)
			if transport == http.DefaultTransport || transport == original || transport.TLSClientConfig.MinVersion != max(minimum, tls.VersionTLS12) {
				t.Fatal("additional CA must use a private transport with TLS 1.2 or stronger")
			}
			if original != nil {
				if client.http == config.HTTPClient || config.HTTPClient.Transport != original || original.TLSClientConfig.MinVersion != minimum || !original.TLSClientConfig.RootCAs.Equal(x509.NewCertPool()) || client.http.Timeout != config.HTTPClient.Timeout {
					t.Fatal("adding a CA mutated the supplied HTTP client or transport")
				}
			}
			if err := client.Send(context.Background(), domain.Message{UserID: 42, Text: "TLS delivery"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCAFileErrorsKeepSafeDiagnosticsAndCauses(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-private-path.pem")
	if _, err := New(Config{Token: "token", CAFile: missing}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CA file read error lost its cause: %v", err)
	}
	invalid := filepath.Join(t.TempDir(), "private-path.pem")
	if err := os.WriteFile(invalid, []byte("private-certificate-content"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := New(Config{Token: "private-token", CAFile: invalid})
	var diagnostic configurationError
	if !errors.As(err, &diagnostic) {
		t.Fatalf("invalid PEM did not produce a safe configuration error: %v", err)
	}
	attrs := diagnostic.LogAttrs()
	if len(attrs) != 2 || attrs[0].Value.String() != "invalid_max_configuration" || attrs[1].Value.String() != "MAX_CA_FILE contains no certificates" {
		t.Fatalf("invalid PEM diagnostic is not safe and actionable: %v", attrs)
	}
}

type customTransport struct{}

func (customTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unexpected network request")
}

func TestCAFileRejectsTransportWithoutTLSConfiguration(t *testing.T) {
	_, err := New(Config{Token: "token", CAFile: "unused.pem", HTTPClient: &http.Client{Transport: customTransport{}}})
	var diagnostic configurationError
	if !errors.As(err, &diagnostic) || diagnostic.Error() != "MAX CA file requires an HTTP transport with configurable TLS" {
		t.Fatalf("unsupported CA transport was not rejected explicitly: %v", err)
	}
}
func TestOversizedResponse(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat(" ", responseLimit+1)))
	})
	if _, err := client.Subscription(context.Background(), 1, 1); err == nil {
		t.Fatal("accepted oversized body")
	}
}
func TestRetryAfter(t *testing.T) {
	for _, test := range []struct {
		header string
		want   time.Duration
	}{
		{"bad", 0}, {"-1", 0}, {"1.5", 0}, {"0", 0}, {"2", 2 * time.Second}, {"300", 300 * time.Second}, {"3600", time.Hour}, {" 86400 ", 24 * time.Hour},
		{"184467440737095516160000", time.Duration(1<<63 - 1)},
	} {
		t.Run(test.header, func(t *testing.T) {
			if got := retryAfter(test.header); got != test.want {
				t.Fatalf("Retry-After %q: got %s, want %s", test.header, got, test.want)
			}
		})
	}
	if retryAfter(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)) != 0 {
		t.Fatal("past date scheduled a retry delay")
	}
	before := time.Now()
	at := before.Add(time.Hour).UTC().Truncate(time.Second)
	delay := retryAfter(at.Format(http.TimeFormat))
	if delay < at.Sub(time.Now()) || delay > at.Sub(before) {
		t.Fatalf("HTTP-date delay was shortened: %s", delay)
	}
}

func TestAnswerCallbackContract(t *testing.T) {
	original := "original:+/&?=callback"
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/answers" || r.URL.Query().Get("callback_id") != original {
			t.Errorf("unexpected callback request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "private-token" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing MAX headers")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 0 {
			t.Errorf("expected empty acknowledgement object: %+v %v", body, err)
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	if err := client.AnswerCallback(context.Background(), 42, original); err != nil {
		t.Fatal(err)
	}
}

func TestAnswerCallbackRejectsFalseOrMissingSuccess(t *testing.T) {
	for _, body := range []string{`{"success":false,"message":"private upstream detail"}`, `{}`, `{"success":null}`, `{`} {
		t.Run(body, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			err := client.AnswerCallback(context.Background(), 42, "callback")
			if err == nil {
				t.Fatal("rejected or malformed acknowledgement was accepted")
			}
			if strings.Contains(err.Error(), "private upstream") {
				t.Fatal("upstream response leaked")
			}
		})
	}
}

func TestAnswerCallbackCancellationPreventsNetworkCalls(t *testing.T) {
	calls := 0
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) { calls++; _, _ = w.Write([]byte(`{"success":true}`)) })
	if err := client.AnswerCallback(context.Background(), 42, "first"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := client.AnswerCallback(ctx, 42, "second"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected canceled pacing, got %v", err)
	}
	if err := client.AnswerCallback(context.Background(), 42, " "); err == nil {
		t.Fatal("blank callback ID accepted")
	}
	if calls != 1 {
		t.Fatalf("canceled/invalid acknowledgement reached MAX: %d", calls)
	}
}
func TestSendingToAnotherDialogDoesNotWaitForThePreviousDialog(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"body":{"mid":"sent"}}}`))
	})
	if err := client.Send(context.Background(), domain.Message{UserID: 42, Text: "first"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := client.Send(ctx, domain.Message{UserID: 43, Text: "second"}); err != nil {
		t.Fatalf("an unrelated dialog was delayed by the previous recipient: %v", err)
	}
}

func TestCallbackAcknowledgementsDoNotShareDifferentDialogLimits(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	if err := client.AnswerCallback(context.Background(), 42, "first"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := client.AnswerCallback(ctx, 43, "second"); err != nil {
		t.Fatalf("callback in another dialog inherited an unrelated limit: %v", err)
	}
}
