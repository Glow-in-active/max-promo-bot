package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"promo/internal/domain"
)

type clockStub struct{ at time.Time }

func (c clockStub) Now() time.Time { return c.at }

type queueRepo struct {
	Repository
	message   *domain.Message
	offer     domain.Offer
	event     *domain.Event
	takeErr   error
	retried   bool
	permanent bool
	detail    string
	finished  bool
}

func (r *queueRepo) TakeMessage(context.Context, time.Time) (*domain.Message, error) {
	return r.message, r.takeErr
}

func (r *queueRepo) RetryMessage(_ context.Context, _ string, detail string, _ time.Time, permanent bool) error {
	r.retried = true
	r.permanent = permanent
	r.detail = detail
	return nil
}

func (r *queueRepo) FinishMessage(context.Context, string) error { r.finished = true; return nil }

func (r *queueRepo) Offer(context.Context, string) (domain.Offer, error) { return r.offer, nil }

func (r *queueRepo) TakeInbox(context.Context, time.Time) (*domain.Event, error) {
	return r.event, r.takeErr
}

func (r *queueRepo) RetryInbox(_ context.Context, _ string, detail string, _ time.Time, permanent bool) error {
	r.retried = true
	r.permanent = permanent
	r.detail = detail
	return nil
}

type sendStub struct {
	err   error
	calls int
}

func (s *sendStub) Subscription(context.Context, int64, int64) (Subscription, error) {
	return SubscriptionUnknown, errors.New("not configured")
}

func (s *sendStub) Send(context.Context, domain.Message) error { s.calls++; return s.err }

type deliveryError struct{ unknown, temporary bool }

func (e deliveryError) Error() string { return "safe delivery failure" }

func (e deliveryError) Uncertain() bool { return e.unknown }

func (e deliveryError) Temporary() bool { return e.temporary }

func TestOutboxDeliveryPolicies(t *testing.T) {
	for _, tc := range []struct {
		name      string
		offer     bool
		attempts  int
		err       error
		permanent bool
		detail    string
	}{
		{"reply_retry", false, 1, deliveryError{true, true}, false, "delivery failed"},
		{"prize_unknown", true, 1, deliveryError{true, true}, true, "delivery uncertain"},
		{"prize_broken_ack", true, 1, deliveryError{true, false}, true, "delivery uncertain"},
		{"rejected", false, 1, deliveryError{false, false}, true, "delivery rejected"},
		{"bounded", false, 5, errors.New("temporary"), true, "delivery failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := &domain.Message{ID: "m", Attempts: tc.attempts}
			if tc.offer {
				message.OfferID = "o"
			}
			repo := &queueRepo{message: message, offer: domain.Offer{Status: "pending"}}
			sender := &sendStub{err: tc.err}
			s := New(repo, sender, clockStub{time.Now()}, domain.CryptoRandom{})
			if err := s.processMessage(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !repo.retried || repo.permanent != tc.permanent || repo.detail != tc.detail || repo.finished {
				t.Fatalf("unexpected retry %+v", repo)
			}
		})
	}
}

func TestOutboxDoesNotResendObsoleteOffers(t *testing.T) {
	for _, status := range []string{"expired", "claimed"} {
		repo := &queueRepo{message: &domain.Message{ID: "m", OfferID: "o"}, offer: domain.Offer{Status: status}}
		sender := &sendStub{}
		s := New(repo, sender, RealClock{}, domain.CryptoRandom{})
		if err := s.processMessage(context.Background()); err != nil {
			t.Fatal(err)
		}
		if sender.calls != 0 || !repo.finished {
			t.Fatal("obsolete offer was sent")
		}
	}
}

func TestOutboxChecksTheClaimDeadlineBeforeSending(t *testing.T) {
	deadline := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for _, offset := range []time.Duration{-time.Second, 0, time.Nanosecond} {
		t.Run(offset.String(), func(t *testing.T) {
			repo := &queueRepo{
				message: &domain.Message{ID: "message", OfferID: "offer", Attempts: 1},
				offer:   domain.Offer{ID: "offer", Status: "notified", Deadline: &deadline},
			}
			sender := &sendStub{err: errors.New("temporary delivery failure")}
			service := New(repo, sender, clockStub{deadline.Add(offset)}, domain.CryptoRandom{})
			if err := service.processMessage(context.Background()); err != nil {
				t.Fatal(err)
			}
			if offset < 0 {
				if sender.calls != 1 || !repo.retried || repo.finished {
					t.Fatal("an available prize notification was discarded")
				}
			} else if sender.calls != 0 || repo.retried || !repo.finished {
				t.Fatal("an expired prize notification reached the provider")
			}
		})
	}
}

func TestOutboxSuccessAndIdle(t *testing.T) {
	repo := &queueRepo{}
	sender := &sendStub{}
	s := New(repo, sender, RealClock{}, domain.CryptoRandom{})
	if err := s.processMessage(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo.message = &domain.Message{ID: "m"}
	if err := s.processMessage(context.Background()); err != nil || !repo.finished {
		t.Fatal(err)
	}
	repo.takeErr = errors.New("db unavailable")
	if !errors.Is(s.processMessage(context.Background()), repo.takeErr) {
		t.Fatal("storage error lost")
	}
}

func TestOutboxCancellationLeavesRecoverableLease(t *testing.T) {
	repo := &queueRepo{message: &domain.Message{ID: "m"}}
	sender := &sendStub{err: context.Canceled}
	s := New(repo, sender, RealClock{}, domain.CryptoRandom{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(s.processMessage(ctx), context.Canceled) {
		t.Fatal("cancellation lost")
	}
	if repo.retried || repo.finished {
		t.Fatal("shutdown modified the delivery outcome")
	}
}

func TestInvalidInboxIsBounded(t *testing.T) {
	repo := &queueRepo{event: &domain.Event{ID: "bad", Attempts: 5}}
	s := New(repo, &sendStub{}, RealClock{}, domain.CryptoRandom{})
	if err := s.processInbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !repo.retried || !repo.permanent {
		t.Fatal("invalid command not dead-lettered")
	}
	repo.event = nil
	if err := s.processInbox(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBackoffIsBounded(t *testing.T) {
	if backoff(0) != 2*time.Second || backoff(1) != 2*time.Second || backoff(3) != 8*time.Second || backoff(100) != 64*time.Second {
		t.Fatal("unbounded backoff")
	}
}

type providerDelayError struct {
	deliveryError
	delay time.Duration
}

func (e providerDelayError) RetryDelay() time.Duration { return e.delay }

type scheduledQueueRepo struct {
	Repository
	message   domain.Message
	next      time.Time
	offer     domain.Offer
	permanent bool
	retried   bool
	detail    string
}

func (r *scheduledQueueRepo) TakeMessage(_ context.Context, now time.Time) (*domain.Message, error) {
	if r.permanent || now.Before(r.next) {
		return nil, nil
	}
	r.message.Attempts++
	copy := r.message
	return &copy, nil
}
func (r *scheduledQueueRepo) RetryMessage(_ context.Context, _ string, detail string, next time.Time, permanent bool) error {
	r.retried = true
	r.next = next
	r.permanent = permanent
	r.detail = detail
	return nil
}
func (r *scheduledQueueRepo) Offer(context.Context, string) (domain.Offer, error) {
	return r.offer, nil
}

type adjustableClock struct{ at time.Time }

func (c *adjustableClock) Now() time.Time { return c.at }

func TestOutboxRespectsProviderRetryAfterBeforeTakingAnotherAttempt(t *testing.T) {
	start := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	deadline := start.Add(time.Hour)
	sentAt := start.Add(-time.Minute)
	repo := &scheduledQueueRepo{message: domain.Message{ID: "message", OfferID: "offer"}, offer: domain.Offer{ID: "offer", Status: "notified", SentAt: &sentAt, Deadline: &deadline}}
	clock := &adjustableClock{at: start}
	sender := &sendStub{err: providerDelayError{deliveryError: deliveryError{temporary: true}, delay: 300 * time.Second}}
	service := New(repo, sender, clock, domain.CryptoRandom{})
	if err := service.processMessage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 || repo.message.Attempts != 1 || repo.permanent || !repo.next.Equal(start.Add(300*time.Second)) {
		t.Fatalf("provider cooldown was ignored: calls=%d attempts=%d next=%s terminal=%v", sender.calls, repo.message.Attempts, repo.next, repo.permanent)
	}
	for _, elapsed := range []time.Duration{2 * time.Second, 30 * time.Second, 299 * time.Second, 300*time.Second - time.Nanosecond} {
		clock.at = start.Add(elapsed)
		if err := service.processMessage(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if sender.calls != 1 || repo.message.Attempts != 1 {
		t.Fatalf("queue consumed attempts during provider cooldown: calls=%d attempts=%d", sender.calls, repo.message.Attempts)
	}
	clock.at = start.Add(300 * time.Second)
	if err := service.processMessage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 2 || repo.message.Attempts != 2 || repo.permanent || !repo.next.Equal(start.Add(600*time.Second)) {
		t.Fatalf("retry was not eligible at provider boundary: calls=%d attempts=%d next=%s terminal=%v", sender.calls, repo.message.Attempts, repo.next, repo.permanent)
	}
	if repo.offer.Deadline == nil || !repo.offer.Deadline.Equal(start.Add(time.Hour)) || repo.offer.SentAt == nil || !repo.offer.SentAt.Equal(start.Add(-time.Minute)) {
		t.Fatal("delivery retry changed the claim deadline")
	}
}

func TestProviderDelayPreservesBackoffAndDeliveryPolicies(t *testing.T) {
	for _, test := range []struct {
		name                            string
		delay                           time.Duration
		attempts                        int
		prize                           bool
		uncertain, temporary, permanent bool
		wantDelay                       time.Duration
		detail                          string
	}{
		{name: "backoff wins", delay: time.Second, attempts: 3, temporary: true, wantDelay: 8 * time.Second, detail: "delivery failed"},
		{name: "negative ignored", delay: -time.Second, attempts: 1, temporary: true, wantDelay: 2 * time.Second, detail: "delivery failed"},
		{name: "provider wins", delay: time.Hour, attempts: 1, temporary: true, wantDelay: time.Hour, detail: "delivery failed"},
		{name: "uncertain prize remains terminal", delay: 300 * time.Second, attempts: 1, prize: true, uncertain: true, temporary: true, permanent: true, wantDelay: 300 * time.Second, detail: "delivery uncertain"},
		{name: "rejection remains terminal", delay: 300 * time.Second, attempts: 1, temporary: false, permanent: true, wantDelay: 300 * time.Second, detail: "delivery rejected"},
		{name: "exhausted attempts remain terminal", delay: 300 * time.Second, attempts: 5, temporary: true, permanent: true, wantDelay: 300 * time.Second, detail: "delivery failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
			repo := &scheduledQueueRepo{message: domain.Message{ID: "message", Attempts: test.attempts - 1}, offer: domain.Offer{Status: "pending"}}
			if test.prize {
				repo.message.OfferID = "offer"
			}
			sender := &sendStub{err: fmt.Errorf("wrapped delivery: %w", providerDelayError{deliveryError: deliveryError{unknown: test.uncertain, temporary: test.temporary}, delay: test.delay})}
			service := New(repo, sender, clockStub{now}, domain.CryptoRandom{})
			if err := service.processMessage(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !repo.retried || repo.permanent != test.permanent || repo.detail != test.detail || !repo.next.Equal(now.Add(test.wantDelay)) {
				t.Fatalf("provider metadata changed delivery policy: %+v", repo)
			}
		})
	}
}

func TestWorkTimeoutRemainsBelowTheQueueLease(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second, 30 * time.Second, time.Minute} {
		service := New(nil, nil, RealClock{}, domain.CryptoRandom{}, WithWorkTimeout(timeout))
		if service.workTimeout != 15*time.Second {
			t.Fatalf("invalid job timeout changed the bounded default: %s", service.workTimeout)
		}
	}
	service := New(nil, nil, RealClock{}, domain.CryptoRandom{}, WithWorkTimeout(50*time.Millisecond))
	if service.workTimeout != 50*time.Millisecond {
		t.Fatal("explicit bounded job timeout was ignored")
	}
}

type blockedInboxRepo struct{ *queueRepo }

func (*blockedInboxRepo) Within(ctx context.Context, _ func(Tx) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestInboxDeadlineLeavesTheAcquiredLeaseRecoverable(t *testing.T) {
	repo := &blockedInboxRepo{queueRepo: &queueRepo{event: &domain.Event{
		ID: "event", Attempts: 1,
		Command: domain.Command{ID: "event", CampaignID: "campaign", UserID: 1, Action: "start"},
	}}}
	service := New(repo, &sendStub{}, RealClock{}, domain.CryptoRandom{}, WithWorkTimeout(20*time.Millisecond))
	if err := service.processInbox(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inbox job had no deadline: %v", err)
	}
	if repo.retried || repo.finished {
		t.Fatal("expired job context changed the durable lease outcome")
	}
}

type blockedSender struct{ sendStub }

func (s *blockedSender) Send(ctx context.Context, _ domain.Message) error {
	s.calls++
	<-ctx.Done()
	return ctx.Err()
}

func TestOutboxDeadlineDoesNotAcknowledgeAnUncertainDelivery(t *testing.T) {
	repo := &queueRepo{message: &domain.Message{ID: "message"}}
	sender := &blockedSender{}
	service := New(repo, sender, RealClock{}, domain.CryptoRandom{}, WithWorkTimeout(20*time.Millisecond))
	if err := service.processMessage(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("outbox job had no deadline: %v", err)
	}
	if sender.calls != 1 || repo.retried || repo.finished {
		t.Fatal("timed-out delivery was acknowledged or its outcome rewritten")
	}
}

type cancelingSender struct {
	sendStub
	cancel context.CancelFunc
}

func (s *cancelingSender) Send(context.Context, domain.Message) error {
	s.cancel()
	return nil
}

func TestOutboxCancellationAfterSendDoesNotAcknowledgeTheJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := &queueRepo{message: &domain.Message{ID: "message"}}
	service := New(repo, &cancelingSender{cancel: cancel}, RealClock{}, domain.CryptoRandom{})
	if err := service.processMessage(ctx); !errors.Is(err, context.Canceled) || repo.finished || repo.retried {
		t.Fatalf("canceled delivery was acknowledged: error=%v finished=%v retried=%v", err, repo.finished, repo.retried)
	}
}

type schedulerQueryRepo struct {
	Repository
	draws  func(context.Context) error
	offers func(context.Context) error
}

func (r *schedulerQueryRepo) DueDraws(ctx context.Context, _ time.Time) ([]domain.Draw, error) {
	return nil, r.draws(ctx)
}

func (r *schedulerQueryRepo) DueOffers(ctx context.Context, _ time.Time) ([]domain.Offer, error) {
	return nil, r.offers(ctx)
}

func TestSchedulerBoundsDueQueriesIndependentlyAndAggregatesErrors(t *testing.T) {
	const timeout = 50 * time.Millisecond
	checkDeadline := func(ctx context.Context) {
		t.Helper()
		deadline, ok := ctx.Deadline()
		if !ok || ctx.Err() != nil || time.Until(deadline) > timeout {
			t.Fatal("scheduler query did not receive a fresh bounded context")
		}
	}
	offerFailure := errors.New("offer lookup unavailable")
	offerCalls := 0
	repo := &schedulerQueryRepo{
		draws: func(ctx context.Context) error {
			checkDeadline(ctx)
			<-ctx.Done()
			return ctx.Err()
		},
		offers: func(ctx context.Context) error {
			offerCalls++
			checkDeadline(ctx)
			return offerFailure
		},
	}
	service := New(repo, &sendStub{}, RealClock{}, domain.CryptoRandom{}, WithWorkTimeout(timeout))
	err := service.Tick(context.Background())
	if offerCalls != 1 || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, offerFailure) {
		t.Fatalf("scheduler skipped the independent query or lost errors: offer calls=%d error=%v", offerCalls, err)
	}
}

func TestSchedulerStopsStartingWorkAfterParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	offerCalls := 0
	repo := &schedulerQueryRepo{
		draws: func(ctx context.Context) error { cancel(); return ctx.Err() },
		offers: func(context.Context) error {
			offerCalls++
			return nil
		},
	}
	service := New(repo, &sendStub{}, RealClock{}, domain.CryptoRandom{})
	if err := service.Tick(ctx); !errors.Is(err, context.Canceled) || offerCalls != 0 {
		t.Fatalf("canceled scheduler started independent work: offer calls=%d error=%v", offerCalls, err)
	}
}
