package httpapi

import (
	"net/http"

	"promo/internal/transport/httpapi/generated"
)

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
