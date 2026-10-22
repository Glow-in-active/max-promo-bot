package httpapi

import (
	"embed"
	"net/http"

	"promo/internal/transport/httpapi/generated"
)

//go:embed assets/banner.svg
var assets embed.FS

func (s *server) DemoBanner(w http.ResponseWriter, _ *http.Request) {
	data, _ := assets.ReadFile("assets/banner.svg")
	w.Header().Set("Content-Type", "image/svg+xml")
	_, _ = w.Write(data)
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
