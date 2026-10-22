package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"promo/internal/domain"
	"promo/internal/observability"
	"promo/internal/transport/httpapi/generated"
)

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	body := generated.Error{}
	body.Error.Code = code
	body.Error.Message = message
	writeJSON(w, status, body)
}

func handleError(w http.ResponseWriter, err error) {
	if fault, ok := domain.FaultOf(err); ok {
		status := http.StatusConflict
		switch fault.Code {
		case domain.Invalid:
			status = http.StatusBadRequest
		case domain.NotFound:
			status = http.StatusNotFound
		case domain.Forbidden:
			status = http.StatusForbidden
		case domain.SubscriptionUnavailable:
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, string(fault.Code), fault.Message)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "Service is temporarily unavailable. Retry the request.")
		return
	}
	slog.LogAttrs(context.Background(), slog.LevelError, "application request failed", observability.ErrorAttrs(err)...)
	writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error.")
}
