package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"promo/internal/observability"
	"promo/internal/transport/httpapi/generated"
)

func (s *server) AcceptMAXUpdate(w http.ResponseWriter, r *http.Request, params generated.AcceptMAXUpdateParams) {
	campaign := s.config.CampaignID
	if s.config.Mode == "demo" || s.config.Mode == "simulator" {
		campaign = params.XDemoCampaign
	}
	if campaign == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Campaign is required.")
		return
	}
	var update generated.AcceptMAXUpdateJSONRequestBody
	if !decode(w, r, &update, false) {
		return
	}
	command, ignored, err := maxUpdate(update).command(campaign)
	if err != nil {
		handleError(w, err)
		return
	}
	if ignored {
		writeJSON(w, http.StatusOK, generated.WebhookAck{Accepted: true, Ignored: true})
		return
	}
	accepted, err := s.repo.PutInbox(r.Context(), command, command.ID)
	if err != nil {
		handleError(w, err)
		return
	}
	if update.UpdateType == "message_callback" {
		s.acknowledgeCallback(r.Context(), command.UserID, update.Callback.CallbackID)
	}
	// MAX requires 200 after durable acceptance. Processing is performed by workers.
	writeJSON(w, http.StatusOK, generated.WebhookAck{Accepted: true, Duplicate: !accepted, EventID: command.ID})
}

func (s *server) acknowledgeCallback(ctx context.Context, userID int64, id string) {
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	if err := s.callbacks.AnswerCallback(ctx, userID, id); err != nil {
		attrs := append(observability.ErrorAttrs(err), slog.Bool("timeout", errors.Is(err, context.DeadlineExceeded)))
		slog.LogAttrs(ctx, slog.LevelWarn, "MAX callback acknowledgement failed", attrs...)
	}
}
