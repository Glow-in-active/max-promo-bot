package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"

	"promo/internal/domain"
	"promo/internal/observability"
)

func TestWorkerLogsDistinguishRetryFailureAndDeliveryWithoutPayloads(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts int
		err      error
		want     string
		level    string
	}{
		{"retry", 1, errors.New("Иван password=secret-token"), "WARN", "debug"},
		{"failed", 5, errors.New("Иван password=secret-token"), "ERROR", "debug"},
		{"delivered", 1, nil, "DEBUG", "debug"},
		{"info hides successful jobs", 1, nil, "", "info"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger, err := observability.New(&output, tc.level)
			if err != nil {
				t.Fatal(err)
			}
			repo := &queueRepo{message: &domain.Message{ID: "message-1", Text: "Иван secret-promo", Attempts: tc.attempts}}
			service := New(repo, &sendStub{err: tc.err}, RealClock{}, domain.CryptoRandom{}, WithLogger(logger))
			if err := service.processMessage(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if output.Len() != 0 {
					t.Fatalf("successful job reached info logs: %s", output.String())
				}
				return
			}
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			var last map[string]any
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
				t.Fatal(err)
			}
			if last["level"] != tc.want || last["message_ref"] != observability.Ref("message-1") {
				t.Fatalf("wrong severity or missing correlation: %v", last)
			}
			if tc.want == "WARN" && last["retry_at"] == nil {
				t.Fatal("retry log has no scheduled time")
			}
			if tc.want == "ERROR" && last["failed"] != true {
				t.Fatal("terminal failure is indistinguishable from a retry")
			}
			for _, r := range output.String() {
				if unicode.Is(unicode.Cyrillic, r) {
					t.Fatalf("Cyrillic reached technical logs: %s", output.String())
				}
			}
			if strings.Contains(output.String(), "secret-") || strings.Contains(output.String(), "password=") {
				t.Fatalf("message or error leaked into logs: %s", output.String())
			}
		})
	}
}

func TestInvalidInboxLogsSafeFailureWithoutPersistingRawError(t *testing.T) {
	var output bytes.Buffer
	logger, err := observability.New(&output, "debug")
	if err != nil {
		t.Fatal(err)
	}
	repo := &queueRepo{event: &domain.Event{ID: "event-1", Attempts: 5}}
	service := New(repo, &sendStub{}, RealClock{}, domain.CryptoRandom{}, WithLogger(logger))
	if err := service.processInbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !repo.permanent || repo.detail != "processing failed" || !strings.Contains(output.String(), `"error_code":"invalid_request"`) || !strings.Contains(output.String(), `"level":"ERROR"`) {
		t.Fatalf("unsafe or incomplete failed inbox record: detail=%q logs=%s", repo.detail, output.String())
	}
}

type retryLogRepository struct {
	queueRepo
	err  error
	next time.Time
}

func (r *retryLogRepository) RetryMessage(ctx context.Context, id, detail string, next time.Time, failed bool) error {
	r.next = next
	if r.err != nil {
		return r.err
	}
	return r.queueRepo.RetryMessage(ctx, id, detail, next, failed)
}

func (r *retryLogRepository) RetryInbox(ctx context.Context, id, detail string, next time.Time, failed bool) error {
	r.next = next
	if r.err != nil {
		return r.err
	}
	return r.queueRepo.RetryInbox(ctx, id, detail, next, failed)
}

func TestWorkerDoesNotLogUnpersistedRetryState(t *testing.T) {
	for _, queue := range []string{"inbox", "outbox"} {
		t.Run(queue, func(t *testing.T) {
			var output bytes.Buffer
			logger, err := observability.New(&output, "debug")
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("database unavailable")
			repo := &retryLogRepository{queueRepo: queueRepo{
				message: &domain.Message{ID: "message", Attempts: 1},
				event:   &domain.Event{ID: "event", Attempts: 1},
			}, err: failure}
			service := New(repo, &sendStub{err: errors.New("provider unavailable")}, RealClock{}, domain.CryptoRandom{}, WithLogger(logger))
			process := service.processInbox
			if queue == "outbox" {
				process = service.processMessage
			}
			if err := process(context.Background()); !errors.Is(err, failure) {
				t.Fatalf("persistence failure was lost: %v", err)
			}
			if strings.Contains(output.String(), "retry_at") || strings.Contains(output.String(), `"failed":`) {
				t.Fatalf("logs promised an unsaved retry state: %s", output.String())
			}
		})
	}
}

type advancingLogClock struct{ now time.Time }

func (c *advancingLogClock) Now() time.Time {
	c.now = c.now.Add(time.Second)
	return c.now
}

func TestRetryLogTimeMatchesSavedSchedule(t *testing.T) {
	var output bytes.Buffer
	logger, err := observability.New(&output, "debug")
	if err != nil {
		t.Fatal(err)
	}
	repo := &retryLogRepository{queueRepo: queueRepo{message: &domain.Message{ID: "message", Attempts: 1}}}
	clock := &advancingLogClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	service := New(repo, &sendStub{err: errors.New("provider unavailable")}, clock, domain.CryptoRandom{}, WithLogger(logger))
	if err := service.processMessage(context.Background()); err != nil {
		t.Fatal(err)
	}
	var record struct {
		RetryAt time.Time `json:"retry_at"`
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &record); err != nil {
		t.Fatal(err)
	}
	if record.RetryAt.IsZero() || !record.RetryAt.Equal(repo.next) {
		t.Fatalf("retry log differs from persisted state: log=%s saved=%s", record.RetryAt, repo.next)
	}
}
