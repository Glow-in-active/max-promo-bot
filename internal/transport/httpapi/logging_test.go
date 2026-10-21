package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"unicode"

	"promo/internal/domain"
	"promo/internal/platform/demo"
)

func captureHTTPLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func completedRequests(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "HTTP request completed" {
			records = append(records, record)
		}
	}
	return records
}

func TestHTTPCompletionRespectsLogLevelAndOmitsHealthyProbes(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo} {
		t.Run(level.String(), func(t *testing.T) {
			output := captureHTTPLogs(t, level)
			h := handler(&repositoryStub{}, &serviceStub{}, "production")
			for _, path := range []string{"/healthz", "/readyz"} {
				if response := request(h, http.MethodGet, path, "", ""); response.Code != http.StatusOK {
					t.Fatalf("probe failed: %s", response.Body.String())
				}
			}
			if output.Len() != 0 {
				t.Fatalf("healthy probes produced logs: %s", output)
			}
			response := request(h, http.MethodGet, "/api/admin/campaigns/Private%50erson/stats?token=PrivateSecret", "", "admin")
			if response.Code != http.StatusOK {
				t.Fatalf("ordinary request failed: %s", response.Body.String())
			}
			if level == slog.LevelInfo {
				if output.Len() != 0 {
					t.Fatalf("debug completion escaped the info threshold: %s", output)
				}
				return
			}
			records := completedRequests(t, output)
			if len(records) != 1 {
				t.Fatalf("expected one completion: %s", output)
			}
			record := records[0]
			if record["level"] != "DEBUG" || record["method"] != "GET" || record["route"] != "/api/admin/campaigns/{campaign}/stats" || record["status"] != float64(200) {
				t.Fatalf("invalid completion metadata: %v", record)
			}
			if duration, ok := record["duration_ms"].(float64); !ok || duration < 0 {
				t.Fatalf("missing request duration: %v", record)
			}
			if strings.Contains(output.String(), "Private") {
				t.Fatalf("completion exposed a path parameter, raw path or query: %s", output)
			}
		})
	}
}

func TestHTTPFailureLogsUseStableRoutesAndSafeMetadata(t *testing.T) {
	output := captureHTTPLogs(t, slog.LevelInfo)
	repo := &repositoryStub{}
	h := handler(repo, &serviceStub{}, "production")
	if response := request(h, http.MethodGet, "/api/admin/campaigns/PrivatePerson/stats?token=PrivateSecret", "", "PrivateBearer"); response.Code != 401 {
		t.Fatalf("unauthenticated request returned %d", response.Code)
	}
	if response := request(h, http.MethodGet, "/PrivatePath?token=PrivateSecret", "", ""); response.Code != 404 {
		t.Fatalf("unknown route returned %d", response.Code)
	}
	repo.error = errors.New("PrivateSQL password=PrivateSecret name=PrivatePerson")
	if response := request(h, http.MethodGet, "/api/admin/campaigns/PrivatePerson/stats", "", "admin"); response.Code != 500 {
		t.Fatalf("application failure returned %d", response.Code)
	}
	privateBody := `{"update_type":"message_created","timestamp":1,"message":{"sender":{"user_id":42},"recipient":{"chat_type":"dialog"},"body":{"mid":"PrivateMID","text":"PrivatePerson"}}}`
	if response := request(h, http.MethodPost, "/webhooks/max?token=PrivateSecret", privateBody, ""); response.Code != 500 {
		t.Fatalf("webhook storage failure returned %d", response.Code)
	}
	repo.pingErr = repo.error
	if response := request(h, http.MethodGet, "/readyz", "", ""); response.Code != 503 {
		t.Fatalf("readiness failure returned %d", response.Code)
	}
	if strings.Contains(output.String(), "Private") {
		t.Fatalf("error logs exposed request or error data: %s", output)
	}
	records := completedRequests(t, output)
	want := []struct {
		level, route string
		status       int
	}{
		{"WARN", "/api/admin/campaigns/{campaign}/stats", 401},
		{"WARN", "unmatched", 404},
		{"ERROR", "/api/admin/campaigns/{campaign}/stats", 500},
		{"ERROR", "/webhooks/max", 500},
		{"ERROR", "/readyz", 503},
	}
	if len(records) != len(want) {
		t.Fatalf("unexpected completions: %s", output)
	}
	for i, expected := range want {
		if records[i]["level"] != expected.level || records[i]["route"] != expected.route || records[i]["status"] != float64(expected.status) {
			t.Fatalf("completion %d: %v", i, records[i])
		}
	}
}

func TestHTTPAPIErrorsStayEnglishAndExcludeBotMessages(t *testing.T) {
	for _, test := range []struct {
		name, method, path, body, token string
		fault                           error
		status                          int
	}{
		{name: "authentication", method: "GET", path: "/api/admin/campaigns/c/stats", status: 401},
		{name: "MAX payload", method: "POST", path: "/webhooks/max", body: strings.Replace(callbackBody, `"payload":"promo"`, `"payload":"PrivatePerson"`, 1), status: 400},
		{name: "schema validation", method: "POST", path: "/_demo/runs", body: `{"scenario":"PrivatePerson"}`, token: "demo", status: 400},
		{name: "business fault", method: "POST", path: "/api/admin/campaigns/c/draws/d/run", token: "admin", fault: domain.UserFail(domain.Forbidden, "Prize offer belongs to another participant.", "Приз предназначен PrivatePerson, PrivateSecret."), status: 403},
		{name: "internal fault", method: "POST", path: "/api/admin/campaigns/c/draws/d/run", token: "admin", fault: errors.New("PrivatePerson PrivateSecret"), status: 500},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := handler(&repositoryStub{}, &serviceStub{err: test.fault}, "demo")
			response := request(h, test.method, test.path, test.body, test.token)
			if response.Code != test.status {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			var body struct {
				Error struct{ Code, Message string }
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Message == "" || body.Error.Code == "" {
				t.Fatalf("invalid technical error: %s", response.Body.String())
			}
			if strings.Contains(response.Body.String(), "Private") || strings.Contains(response.Body.String(), "user_message") || strings.ContainsFunc(body.Error.Message, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) }) {
				t.Fatalf("API error contains bot prose or private data: %s", response.Body.String())
			}
		})
	}
}

type panickingRepository struct{ *repositoryStub }

func (*panickingRepository) Campaign(context.Context, string) (domain.Campaign, error) {
	panic("PrivatePerson PrivateSecret")
}

func TestHTTPCompletionIncludesImplicitAndRecoveredPanicResponses(t *testing.T) {
	output := captureHTTPLogs(t, slog.LevelDebug)
	h := handler(&repositoryStub{}, &serviceStub{}, "production")
	response := request(h, http.MethodGet, "/assets/banner.svg", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("asset returned %d", response.Code)
	}
	messenger := demo.New()
	broken := New(&serviceStub{}, &panickingRepository{repositoryStub: &repositoryStub{}}, messenger, messenger, Config{Mode: "production", AdminToken: "admin"})
	response = request(broken, http.MethodGet, "/api/admin/campaigns/PrivatePerson/stats", "", "admin")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "Private") || strings.Contains(output.String(), "Private") {
		t.Fatalf("panic was not safely recovered: status=%d body=%s logs=%s", response.Code, response.Body.String(), output)
	}
	records := completedRequests(t, output)
	if len(records) != 2 || records[0]["status"] != float64(http.StatusOK) || records[0]["route"] != "/assets/banner.svg" || records[1]["status"] != float64(http.StatusInternalServerError) || records[1]["level"] != "ERROR" {
		t.Fatalf("implicit or panic status was not observed: %s", output)
	}
}
