package demo

import (
	"context"
	"testing"

	"promo/internal/app"
	"promo/internal/domain"
)

func TestSubscriptionsAreIsolated(t *testing.T) {
	messenger := New()
	messenger.SetSubscription(1, 42, "no")
	messenger.SetSubscription(2, 42, "error")
	for _, test := range []struct {
		channel int64
		want    app.Subscription
		failure bool
	}{{1, app.SubscriptionNo, false}, {2, app.SubscriptionUnknown, true}, {3, app.SubscriptionYes, false}} {
		got, err := messenger.Subscription(context.Background(), test.channel, 42)
		if got != test.want || (err != nil) != test.failure {
			t.Fatalf("channel %d: got %v, %v", test.channel, got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := messenger.Subscription(ctx, 1, 42); err == nil {
		t.Fatal("canceled subscription succeeded")
	}
	if err := messenger.AnswerCallback(ctx, 42, "id"); err == nil {
		t.Fatal("canceled acknowledgement succeeded")
	}
	if err := messenger.AnswerCallback(context.Background(), 42, "id"); err != nil {
		t.Fatal(err)
	}
	if err := messenger.Send(ctx, domain.Message{}); err == nil {
		t.Fatal("canceled send succeeded")
	}
	if err := messenger.Send(context.Background(), domain.Message{}); err != nil {
		t.Fatal(err)
	}
}
