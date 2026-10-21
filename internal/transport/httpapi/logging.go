package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type requestLogKey struct{}

type requestLog struct{ route string }

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func recordRoute(r *http.Request, route string) {
	if record, ok := r.Context().Value(requestLogKey{}).(*requestLog); ok {
		record.route = route
	}
}

func logMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

func logHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		record := &requestLog{route: "unmatched"}
		r = r.WithContext(context.WithValue(r.Context(), requestLogKey{}, record))
		response := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(response, r)
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		if r.Pattern != "" {
			record.route = r.Pattern
			if _, path, ok := strings.Cut(r.Pattern, " "); ok {
				record.route = path
			}
		}
		if status < 400 && (record.route == "/healthz" || record.route == "/readyz") {
			return
		}
		level := slog.LevelDebug
		if status >= 500 {
			level = slog.LevelError
		} else if status >= 400 {
			level = slog.LevelWarn
		}
		slog.LogAttrs(r.Context(), level, "HTTP request completed",
			slog.String("method", logMethod(r.Method)), slog.String("route", record.route),
			slog.Int("status", status), slog.Float64("duration_ms", float64(time.Since(started))/float64(time.Millisecond)))
	})
}
