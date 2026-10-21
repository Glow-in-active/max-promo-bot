package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"promo/internal/config"
	"promo/internal/domain"
	"promo/internal/platform/maxapi"
)

func TestBotPlatformWiresCallbacksForEachMode(t *testing.T) {
	demoPlatform, err := newBotPlatform(config.Config{Mode: "demo", WebhookSecret: "configured-webhook-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if demoPlatform.demo == nil || demoPlatform.messenger != demoPlatform.demo || demoPlatform.callbacks != demoPlatform.demo {
		t.Fatal("demo uses an external callback responder")
	}
	if demoPlatform.webhookSecret != "configured-webhook-secret" {
		t.Fatal("configured webhook secret was changed")
	}
	realPlatform, err := newBotPlatform(config.Config{Mode: "production", MAXToken: "test-token", MAXURL: "https://platform-api2.max.ru"})
	if err != nil {
		t.Fatal(err)
	}
	if realPlatform.demo != nil {
		t.Fatal("production selected demo")
	}
	client, ok := realPlatform.messenger.(*maxapi.Client)
	if !ok || realPlatform.callbacks != client {
		t.Fatal("production messenger and callback responder must share the real MAX client")
	}
}

func TestSimulatorUsesTheConfiguredExternalAPI(t *testing.T) {
	messageCalls, callbackCalls := 0, 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "local-max-token" {
			t.Error("simulator token was not sent to the external API")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/messages":
			messageCalls++
			if r.Method != http.MethodPost || r.URL.Query().Get("user_id") != "42" {
				t.Error("outgoing message has an invalid recipient or method")
			}
			_, _ = w.Write([]byte(`{"message":{"body":{"mid":"message-id"}}}`))
		case "/answers":
			callbackCalls++
			if r.Method != http.MethodPost || r.URL.Query().Get("callback_id") != "callback-id" {
				t.Error("callback acknowledgement has an invalid identity or method")
			}
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	platform, err := newBotPlatform(config.Config{Mode: "simulator", MAXURL: api.URL, MAXToken: "local-max-token", WebhookSecret: "local-webhook-secret"})
	if err != nil {
		t.Fatal(err)
	}
	client, ok := platform.messenger.(*maxapi.Client)
	if !ok || platform.callbacks != client || platform.demo != nil {
		t.Fatal("simulator must use one real MAX client without an in-memory messenger")
	}
	if platform.webhookSecret != "local-webhook-secret" {
		t.Fatal("configured simulator webhook secret was changed")
	}
	if err := platform.messenger.Send(context.Background(), domain.Message{UserID: 42, Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := platform.callbacks.AnswerCallback(context.Background(), 42, "callback-id"); err != nil {
		t.Fatal(err)
	}
	if messageCalls != 1 || callbackCalls != 1 {
		t.Fatalf("external API calls: messages=%d callbacks=%d", messageCalls, callbackCalls)
	}
}

func TestHealthcheckUsesConfiguredAddressWithoutDatabaseOrMAX(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/readyz" || r.Method != http.MethodGet {
			t.Errorf("unexpected probe: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("HTTP_ADDR", server.Listener.Addr().String())
	t.Setenv("POSTGRES_PORT", "invalid")
	t.Setenv("MODE", "production")
	t.Setenv("LOG_LEVEL", "invalid")
	t.Setenv("MAX_TOKEN", "")
	t.Setenv("ADMIN_TOKEN", "")
	if err := run([]string{"healthcheck"}); err != nil || calls != 1 {
		t.Fatalf("configured standalone probe failed: calls=%d error=%v", calls, err)
	}
}

func TestCommandsRejectUnexpectedArgumentsBeforeLoadingConfiguration(t *testing.T) {
	t.Setenv("HTTP_ADDR", "invalid")
	t.Setenv("POSTGRES_PORT", "invalid")
	for _, args := range [][]string{
		{"healthcheck", "http://example.org"}, {"serve", "extra"},
		{"migrate", "extra"}, {"setup"}, {"setup", "one.json", "two.json"}, {"unknown"},
	} {
		err := run(args)
		var usage usageError
		if !errors.As(err, &usage) {
			t.Fatalf("arguments %q returned %v instead of usage", args, err)
		}
	}
}

func TestSetupValidatesInputBeforeConnectingToDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "campaign.json")
	if err := os.WriteFile(path, []byte(`{} {}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTP_ADDR", "invalid")
	t.Setenv("POSTGRES_PORT", "invalid")
	err := run([]string{"setup", path})
	if err == nil || err.Error() != "campaign file must contain exactly one JSON document" {
		t.Fatalf("setup did not reject extra JSON before infrastructure configuration: %v", err)
	}
}
