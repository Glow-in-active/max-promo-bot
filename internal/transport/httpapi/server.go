package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
	"promo/internal/observability"
	"promo/internal/platform/demo"
	"promo/internal/transport/httpapi/generated"
)

const maxBodyBytes = 64 << 10

//go:embed assets/banner.svg
var assets embed.FS

type Config struct{ Mode, AdminToken, DemoToken, WebhookSecret, CampaignID, BannerURL string }

type Service interface {
	RunDraw(context.Context, string, string, string) (domain.Draw, error)
	ApproveDraw(context.Context, string, string, string) (domain.Draw, error)
	ExpireOffer(context.Context, string) error
	ConfirmDelivery(context.Context, string, string) error
	RetryDelivery(context.Context, string, string) error
	ListOffers(context.Context, string, string) ([]domain.Offer, error)
	Tick(context.Context) error
	Now() time.Time
}

type CallbackResponder interface {
	AnswerCallback(context.Context, int64, string) error
}

type server struct {
	service   Service
	repo      app.Repository
	demo      *demo.Messenger
	config    Config
	callbacks CallbackResponder
}

func New(service Service, repo app.Repository, messenger *demo.Messenger, callbacks CallbackResponder, config Config) http.Handler {
	if callbacks == nil {
		panic("callback responder is required")
	}
	s := &server{service: service, repo: repo, demo: messenger, callbacks: callbacks, config: config}
	handler := s.contractHandler()
	return withRequestTimeout(logHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(r.Context(), "HTTP handler panic", "method", logMethod(r.Method))
				writeError(w, http.StatusInternalServerError, "internal_error", "Internal server error.")
			}
		}()
		if config.Mode != "demo" && config.Mode != "simulator" && (r.URL.Path == "/_demo" || strings.HasPrefix(r.URL.Path, "/_demo/")) {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		handler.ServeHTTP(w, r)
	})))
}

func withRequestTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *server) Liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, generated.HealthStatus{Status: generated.Ok})
}
func (s *server) DemoBanner(w http.ResponseWriter, _ *http.Request) {
	data, _ := assets.ReadFile("assets/banner.svg")
	w.Header().Set("Content-Type", "image/svg+xml")
	_, _ = w.Write(data)
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

func authenticated(actual, expected string) bool {
	a, b := sha256.Sum256([]byte(actual)), sha256.Sum256([]byte(expected))
	return expected != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return token
}

func decode(w http.ResponseWriter, r *http.Request, target any, strict bool) bool {
	if media := strings.Split(r.Header.Get("Content-Type"), ";")[0]; media != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	if strict {
		decoder.DisallowUnknownFields()
	}
	err := decoder.Decode(target)
	if err == nil {
		var extra any
		if next := decoder.Decode(&extra); next != io.EOF {
			err = errors.New("expected exactly one JSON value")
		}
	}
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large.")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON or request fields.")
		}
		return false
	}
	return true
}

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

func (s *server) ListDraws(w http.ResponseWriter, r *http.Request, campaign generated.Campaign) {
	if !s.campaignExists(w, r, campaign) {
		return
	}
	result, err := s.repo.Draws(r.Context(), campaign)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, drawListModel(result))
}
func (s *server) RunDraw(w http.ResponseWriter, r *http.Request, campaign generated.Campaign, draw generated.DrawParameter) {
	result, err := s.service.RunDraw(r.Context(), campaign, draw, "admin")
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, drawModel(result))
}
func (s *server) ApproveDraw(w http.ResponseWriter, r *http.Request, campaign generated.Campaign, draw generated.DrawParameter) {
	result, err := s.service.ApproveDraw(r.Context(), campaign, draw, "admin")
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, drawModel(result))
}
func (s *server) CampaignStats(w http.ResponseWriter, r *http.Request, campaign generated.Campaign) {
	if !s.campaignExists(w, r, campaign) {
		return
	}
	result, err := s.repo.Stats(r.Context(), campaign)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statsModel(result))
}
func (s *server) InspectOffer(w http.ResponseWriter, r *http.Request, offer generated.OfferParameter) {
	result, err := s.repo.Offer(r.Context(), offer)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, offerModel(result))
}
func (s *server) ListDrawOffers(w http.ResponseWriter, r *http.Request, campaign generated.Campaign, draw generated.DrawParameter) {
	result, err := s.service.ListOffers(r.Context(), campaign, draw)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, offerListModel(result))
}
func (s *server) RetryOfferDelivery(w http.ResponseWriter, r *http.Request, offer generated.OfferParameter) {
	if err := s.service.RetryDelivery(r.Context(), offer, "admin-api"); err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, generated.ActionStatus{Status: generated.ActionStatusStatusQueued})
}

func (s *server) ExpireOffer(w http.ResponseWriter, r *http.Request, offer generated.OfferParameter) {
	if err := s.service.ExpireOffer(r.Context(), offer); err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, generated.ActionStatus{Status: generated.ActionStatusStatusProcessed})
}
func (s *server) ConfirmDelivery(w http.ResponseWriter, r *http.Request, offer generated.OfferParameter) {
	if err := s.service.ConfirmDelivery(r.Context(), offer, "admin-api"); err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, generated.ActionStatus{Status: generated.ActionStatusStatusDelivered})
}
func (s *server) ProcessDueWork(w http.ResponseWriter, r *http.Request) {
	if err := s.service.Tick(r.Context()); err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, generated.ActionStatus{Status: generated.ActionStatusStatusProcessed})
}
func (s *server) campaignExists(w http.ResponseWriter, r *http.Request, campaign string) bool {
	_, err := s.repo.Campaign(r.Context(), campaign)
	if err != nil {
		handleError(w, err)
		return false
	}
	return true
}

func (s *server) CreateDemoRun(w http.ResponseWriter, r *http.Request) {
	var input generated.CreateDemoRunJSONRequestBody
	if !decode(w, r, &input, true) {
		return
	}
	switch input.Scenario {
	case "standard", "exhausted", "drawable", "final", "expired":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "Unknown demo scenario.")
		return
	}
	run, err := s.repo.CreateDemo(r.Context(), string(input.Scenario), s.service.Now())
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, demoModel(run))
}
func (s *server) DemoMessages(w http.ResponseWriter, r *http.Request, campaign generated.Campaign, params generated.DemoMessagesParams) {
	if !s.campaignExists(w, r, campaign) {
		return
	}
	after := params.After
	if len(after) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid cursor.")
		return
	}
	result, err := s.repo.Messages(r.Context(), campaign, after)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, messageListModel(result))
}
func (s *server) SetDemoSubscription(w http.ResponseWriter, r *http.Request, campaign generated.Campaign) {
	if s.config.Mode != "demo" || s.demo == nil {
		writeError(w, http.StatusNotFound, "not_found", "Route not found.")
		return
	}
	var input generated.SetDemoSubscriptionJSONRequestBody
	if !decode(w, r, &input, true) {
		return
	}
	if input.UserID <= 0 || (input.Status != "yes" && input.Status != "no" && input.Status != "error") {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid user or subscription status.")
		return
	}
	day, err := s.repo.CurrentDay(r.Context(), campaign, s.service.Now())
	if err != nil {
		handleError(w, err)
		return
	}
	s.demo.SetSubscription(day.ChannelID, input.UserID, string(input.Status))
	writeJSON(w, http.StatusOK, generated.ActionStatus{Status: generated.ActionStatusStatus(input.Status)})
}

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

func eventID(campaign, key string) string {
	sum := sha256.Sum256([]byte(campaign + ":" + key))
	return hex.EncodeToString(sum[:])
}
