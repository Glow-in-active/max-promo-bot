package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"unicode"

	"promo/internal/domain"
)

func TestConfiguredLevelFiltersStructuredEvents(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			var output bytes.Buffer
			logger, err := New(&output, level)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
				logger.LogAttrs(context.Background(), event, "test event", slog.String("worker", "outbox"))
			}
			want := map[string]int{"debug": 4, "info": 3, "warn": 2, "error": 1}[level]
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			if len(lines) != want {
				t.Fatalf("level %s emitted %d events, want %d", level, len(lines), want)
			}
			for _, line := range lines {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["worker"] != "outbox" || record["msg"] != "test event" || record["time"] == "" || record["level"] == "" {
					t.Fatalf("incomplete log record: %v", record)
				}
			}
		})
	}
	for _, level := range []string{"", "trace", "info+2", "секрет"} {
		if _, err := New(&bytes.Buffer{}, level); err == nil || strings.Contains(err.Error(), level) && level == "секрет" {
			t.Fatalf("invalid level was accepted or reflected: %q", level)
		}
	}
}

type stateError struct{ state string }

func (e stateError) Error() string    { return "private database value: Иван password=secret" }
func (e stateError) SQLState() string { return e.state }

type upstreamError struct{ status int }

func (e upstreamError) Error() string   { return "secret-token from upstream" }
func (e upstreamError) HTTPStatus() int { return e.status }

func TestErrorMetadataDoesNotLogMessagesOrCredentials(t *testing.T) {
	tests := []struct {
		err  error
		key  string
		want any
	}{
		{fmt.Errorf("private context: %w", domain.Fail(domain.Invalid, "Иван secret-token")), "error_code", "invalid_request"},
		{fmt.Errorf("wrapped: %w", stateError{"23505"}), "sqlstate", "23505"},
		{errors.Join(errors.New("secret-token"), context.DeadlineExceeded), "error_code", "deadline_exceeded"},
		{context.Canceled, "error_code", "canceled"},
		{errors.New("postgres://user:secret-token@db/Иван"), "error_type", "*errors.errorString"},
		{stateError{"секрет"}, "error_type", "observability.stateError"},
		{domain.Fail(domain.Code("секрет"), "secret-token"), "error_type", "*domain.Fault"},
		{fmt.Errorf("wrapped: %w", upstreamError{429}), "upstream_status", float64(429)},
	}
	for _, tt := range tests {
		var output bytes.Buffer
		logger, err := New(&output, "debug")
		if err != nil {
			t.Fatal(err)
		}
		logger.LogAttrs(context.Background(), slog.LevelError, "operation failed", ErrorAttrs(tt.err)...)
		var record map[string]any
		if err := json.Unmarshal(output.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record[tt.key] != tt.want {
			t.Fatalf("missing safe metadata: %v", record)
		}
		for _, r := range output.String() {
			if unicode.Is(unicode.Cyrillic, r) {
				t.Fatalf("Cyrillic reached the log: %s", output.String())
			}
		}
		if strings.Contains(output.String(), "secret-token") || strings.Contains(output.String(), "password=") || strings.Contains(output.String(), "private context") {
			t.Fatalf("private error data reached the log: %s", output.String())
		}
	}
	if len(ErrorAttrs(nil)) != 0 {
		t.Fatal("nil error produced metadata")
	}
}

func TestResourceReferencesAreStableAndDoNotExposeNames(t *testing.T) {
	first := Ref("Акция-Иван")
	if first == "" || first != Ref("Акция-Иван") || first == Ref("Акция-Пётр") {
		t.Fatal("resource references are unstable or ambiguous")
	}
	if len(first) != 24 || !asciiCode(first) || strings.Contains(first, "Иван") {
		t.Fatalf("resource reference exposed its input: %q", first)
	}
}

func TestStartupErrorMetadataExplainsFileAndJSONFailuresWithoutInputs(t *testing.T) {
	var syntaxTarget any
	syntaxErr := json.Unmarshal([]byte(`{"secret-token":"Иван",}`), &syntaxTarget)
	var typeTarget struct {
		Value int `json:"private-field-Иван"`
	}
	typeErr := json.Unmarshal([]byte(`{"private-field-Иван":"secret-token"}`), &typeTarget)
	for _, tc := range []struct {
		name string
		err  error
		code string
		op   string
	}{
		{"missing", &os.PathError{Op: "open", Path: "/Иван/secret-token.json", Err: os.ErrNotExist}, "file_not_found", "open"},
		{"permission", &os.PathError{Op: "read", Path: "/Иван/secret-token.pem", Err: os.ErrPermission}, "permission_denied", "read"},
		{"unsafe operation", &os.PathError{Op: "Иван secret-token", Path: "/secret-token", Err: os.ErrNotExist}, "file_not_found", ""},
		{"other file failure", &os.PathError{Op: "stat", Path: "/Иван/secret-token", Err: errors.New("private failure")}, "file_error", "stat"},
		{"syntax", syntaxErr, "invalid_json", ""},
		{"type", typeErr, "invalid_json_type", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger, err := New(&output, "error")
			if err != nil {
				t.Fatal(err)
			}
			wrapped := fmt.Errorf("Иван secret-token: %w", tc.err)
			logger.LogAttrs(context.Background(), slog.LevelError, "application stopped", ErrorAttrs(wrapped)...)
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["error_code"] != tc.code {
				t.Fatalf("startup failure lost its cause: %v", record)
			}
			if tc.op != "" && record["file_operation"] != tc.op {
				t.Fatalf("file operation was lost: %v", record)
			}
			if tc.op == "" && record["file_operation"] != nil {
				t.Fatalf("unexpected file operation: %v", record)
			}
			if tc.name == "syntax" || tc.name == "type" {
				if offset, ok := record["json_offset"].(float64); !ok || offset <= 0 {
					t.Fatalf("JSON failure has no byte offset: %v", record)
				}
			}
			for _, private := range []string{"secret-token", "private-field", "private failure"} {
				if strings.Contains(output.String(), private) {
					t.Fatalf("private startup input reached logs: %s", output.String())
				}
			}
			for _, r := range output.String() {
				if unicode.Is(unicode.Cyrillic, r) {
					t.Fatalf("Cyrillic reached startup logs: %s", output.String())
				}
			}
		})
	}
}
