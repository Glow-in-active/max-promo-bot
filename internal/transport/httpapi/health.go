package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"promo/internal/observability"
	"promo/internal/transport/httpapi/generated"
)

func (s *server) Liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, generated.HealthStatus{Status: generated.Ok})
}

func (s *server) Readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.repo.Ping(ctx); err != nil {
		slog.LogAttrs(r.Context(), slog.LevelError, "HTTP readiness check failed", observability.ErrorAttrs(err)...)
		writeError(w, http.StatusServiceUnavailable, "not_ready", "Database is unavailable.")
		return
	}
	writeJSON(w, http.StatusOK, generated.HealthStatus{Status: generated.Ready})
}
