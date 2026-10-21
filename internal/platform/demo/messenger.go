package demo

import (
	"context"
	"errors"
	"sync"

	"promo/internal/app"
	"promo/internal/domain"
)

type key struct{ channelID, userID int64 }

type Messenger struct {
	mu            sync.RWMutex
	subscriptions map[key]string
}

func New() *Messenger { return &Messenger{subscriptions: make(map[key]string)} }

func (m *Messenger) SetSubscription(channelID, userID int64, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subscriptions[key{channelID, userID}] = status
}

func (m *Messenger) Subscription(ctx context.Context, channelID, userID int64) (app.Subscription, error) {
	if err := ctx.Err(); err != nil {
		return app.SubscriptionUnknown, err
	}
	m.mu.RLock()
	status := m.subscriptions[key{channelID, userID}]
	m.mu.RUnlock()
	switch status {
	case "no":
		return app.SubscriptionNo, nil
	case "error":
		return app.SubscriptionUnknown, errors.New("demo: subscription service unavailable")
	default:
		return app.SubscriptionYes, nil
	}
}

func (m *Messenger) Send(ctx context.Context, _ domain.Message) error { return ctx.Err() }

func (m *Messenger) AnswerCallback(ctx context.Context, _ int64, _ string) error { return ctx.Err() }
