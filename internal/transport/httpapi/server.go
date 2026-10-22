package httpapi

import (
	"context"
	"net/http"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
	"promo/internal/platform/demo"
)

type Config struct {
	Mode          string
	AdminToken    string
	DemoToken     string
	WebhookSecret string
	CampaignID    string
	BannerURL     string
}

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
	server := &server{
		service:   service,
		repo:      repo,
		demo:      messenger,
		callbacks: callbacks,
		config:    config,
	}
	return withRequestTimeout(logHTTP(server.withHTTPGuards(server.contractHandler())))
}
