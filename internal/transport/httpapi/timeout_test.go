package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"promo/internal/domain"
	"promo/internal/platform/demo"
)

type waitingRepository struct {
	repositoryStub
	observed context.Context
	wait     bool
}

func (r *waitingRepository) Stats(ctx context.Context, _ string) (domain.Stats, error) {
	r.observed = ctx
	if r.wait {
		<-ctx.Done()
		return domain.Stats{}, ctx.Err()
	}
	return domain.Stats{}, nil
}

func TestHTTPRequestsHaveBoundedDatabaseContext(t *testing.T) {
	repo := &waitingRepository{}
	h := New(&serviceStub{}, repo, demo.New(), demo.New(), Config{AdminToken: "admin"})
	response := request(h, http.MethodGet, "/api/admin/campaigns/campaign/stats", "", "admin")
	if response.Code != http.StatusOK || repo.observed == nil {
		t.Fatalf("statistics request failed: HTTP %d", response.Code)
	}
	deadline, ok := repo.observed.Deadline()
	if !ok || time.Until(deadline) > 10*time.Second || time.Until(deadline) <= 0 {
		t.Fatal("database operation has no bounded request deadline")
	}
	if !errors.Is(repo.observed.Err(), context.Canceled) {
		t.Fatal("completed HTTP request retained its context resources")
	}
}

func TestHTTPPreservesEarlierCancellationAndContinuesServing(t *testing.T) {
	repo := &waitingRepository{wait: true}
	h := New(&serviceStub{}, repo, demo.New(), demo.New(), Config{AdminToken: "admin"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/admin/campaigns/campaign/stats", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer admin")
	response := httptest.NewRecorder()
	started := time.Now()
	h.ServeHTTP(response, r)
	if response.Code != http.StatusServiceUnavailable || time.Since(started) > time.Second || !errors.Is(repo.observed.Err(), context.DeadlineExceeded) {
		t.Fatalf("cancellation was lost: HTTP %d, context=%v", response.Code, repo.observed.Err())
	}
	if probe := request(h, http.MethodGet, "/healthz", "", ""); probe.Code != http.StatusOK {
		t.Fatal("cancelled database operation stopped HTTP service")
	}
}
