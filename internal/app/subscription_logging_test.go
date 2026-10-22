package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"promo/internal/app"
	"promo/internal/domain"
	"promo/internal/observability"
	"promo/internal/platform/maxapi"
)

type subscriptionLogRepository struct {
	app.Repository
	day domain.Day
	err error
}

func (r *subscriptionLogRepository) CurrentDay(context.Context, string, time.Time) (domain.Day, error) {
	return r.day, nil
}

func (r *subscriptionLogRepository) Within(context.Context, func(app.Tx) error) error {
	return r.err
}

func TestSubscriptionFailureLogsSafeProviderDiagnostics(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, 0} {
		name := http.StatusText(status)
		if status == 0 {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == 0 {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte("Иван secret-response password=private"))
			}))
			defer server.Close()
			timeout := time.Second
			if status == 0 {
				timeout = 50 * time.Millisecond
			}
			platform, err := maxapi.New(maxapi.Config{
				Token: "secret-max-token", BaseURL: server.URL,
				HTTPClient: &http.Client{Timeout: timeout},
			})
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			logger, err := observability.New(&output, "info")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			stop := errors.New("transaction boundary reached")
			repo := &subscriptionLogRepository{day: domain.Day{
				ID: "secret-day", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour),
				ChannelID: -9911223344, Partner: "Иван secret-channel",
			}, err: stop}
			service := app.New(repo, platform, app.RealClock{}, domain.CryptoRandom{}, app.WithLogger(logger))
			command := domain.Command{ID: "событие-secret-event", CampaignID: "акция-secret-campaign", UserID: 9900001234, Action: "check", OccurredAt: now}
			if _, err := service.Execute(context.Background(), command); !errors.Is(err, stop) {
				t.Fatalf("subscription failure prevented the business transaction: %v", err)
			}
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatalf("expected one JSON warning: %v; %s", err, output.String())
			}
			if record["level"] != "WARN" || record["msg"] != "subscription check failed" || record["error_type"] != "*maxapi.Error" {
				t.Fatalf("missing failure diagnostics: %v", record)
			}
			if record["event_ref"] != observability.Ref(command.ID) || record["campaign_ref"] != observability.Ref(command.CampaignID) {
				t.Fatalf("failure cannot be correlated with its event: %v", record)
			}
			if status == 0 {
				if record["error_code"] != "deadline_exceeded" {
					t.Fatalf("timeout is indistinguishable from other failures: %v", record)
				}
			} else if record["upstream_status"] != float64(status) {
				t.Fatalf("MAX status was lost: %v", record)
			}
			for _, private := range []string{"secret-", "password=", "9911223344", "9900001234", server.URL} {
				if strings.Contains(output.String(), private) {
					t.Fatalf("private data reached subscription logs: %s", output.String())
				}
			}
			for _, r := range output.String() {
				if unicode.Is(unicode.Cyrillic, r) {
					t.Fatalf("Cyrillic reached subscription logs: %s", output.String())
				}
			}
		})
	}
}
