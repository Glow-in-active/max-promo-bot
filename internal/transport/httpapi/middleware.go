package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const maxBodyBytes = 64 << 10

func (s *server) withHTTPGuards(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(r.Context(), "HTTP handler panic", "method", logMethod(r.Method))
				writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error.")
			}
		}()

		if s.config.Mode != "demo" && s.config.Mode != "simulator" &&
			(r.URL.Path == "/_demo" || strings.HasPrefix(r.URL.Path, "/_demo/")) {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func withRequestTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
