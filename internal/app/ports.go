package app

import (
	"context"
	"time"

	"promo/internal/domain"
)

type Clock interface{ Now() time.Time }
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now().UTC() }

type Subscription int

const (
	SubscriptionUnknown Subscription = iota
	SubscriptionNo
	SubscriptionYes
)

type Messenger interface {
	Subscription(context.Context, int64, int64) (Subscription, error)
	Send(context.Context, domain.Message) error
}

// Tx is scoped to one transaction. Lock order: campaign, draw, user, pool, slot.
type Tx interface {
	Campaign(context.Context, string, bool) (domain.Campaign, error)
	SetCampaignClosed(context.Context, string) error
	DayAt(context.Context, string, time.Time) (domain.Day, error)
	Days(context.Context, string) ([]domain.Day, error)
	UserForUpdate(context.Context, int64, time.Time) (domain.User, error)
	TouchCampaignUser(context.Context, string, int64, time.Time) error
	PendingClaim(context.Context, int64) (string, string, error)
	SetPendingClaim(context.Context, int64, string, string) error
	ClearPendingClaim(context.Context, int64) error
	Activities(context.Context, string, int64) ([]domain.Activity, error)
	AddActivity(context.Context, string, int64, domain.Day, string, time.Time) error
	Assignment(context.Context, string, int64) (*domain.Promo, error)
	LockPool(context.Context, string) error
	NextPromo(context.Context, string) (*domain.Promo, error)
	AssignPromo(context.Context, string, int64, time.Time) error
	RecordPromoRequest(context.Context, string, int64, time.Time) error
	HasEntry(context.Context, string, int64) (bool, error)
	AddEntry(context.Context, string, int64, time.Time) error
	Draw(context.Context, string, bool) (domain.Draw, error)
	Candidates(context.Context, domain.Draw) ([]domain.Candidate, error)
	SaveDraw(context.Context, domain.Draw) error
	CreateSlot(context.Context, string, string, int) error
	LockSlot(context.Context, string) error
	Offer(context.Context, string) (domain.Offer, error)
	Offers(context.Context, string) ([]domain.Offer, error)
	SaveOffer(context.Context, domain.Offer) error
	RetryOfferMessage(context.Context, string, time.Time) error
	Enqueue(context.Context, domain.Message, string, time.Time) error
	Audit(context.Context, string, string, string, time.Time) error
	InboxDone(context.Context, string) (bool, error)
	CompleteInbox(context.Context, string) error
}

type Repository interface {
	Within(context.Context, func(Tx) error) error
	Campaign(context.Context, string) (domain.Campaign, error)
	CurrentDay(context.Context, string, time.Time) (domain.Day, error)
	Draw(context.Context, string) (domain.Draw, error)
	Draws(context.Context, string) ([]domain.Draw, error)
	Offer(context.Context, string) (domain.Offer, error)
	Activities(context.Context, string, int64) ([]domain.Activity, error)
	Stats(context.Context, string) (domain.Stats, error)
	Export(context.Context, string) (domain.ExportSnapshot, error)
	PutInbox(context.Context, domain.Command, string) (bool, error)
	TakeInbox(context.Context, time.Time) (*domain.Event, error)
	RetryInbox(context.Context, string, string, time.Time, bool) error
	TakeMessage(context.Context, time.Time) (*domain.Message, error)
	FinishMessage(context.Context, string) error
	RetryMessage(context.Context, string, string, time.Time, bool) error
	DueDraws(context.Context, time.Time) ([]domain.Draw, error)
	DueOffers(context.Context, time.Time) ([]domain.Offer, error)
	CreateDemo(context.Context, string, time.Time) (domain.DemoRun, error)
	Messages(context.Context, string, string) ([]domain.Message, error)
	Ping(context.Context) error
}
