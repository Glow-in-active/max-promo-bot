package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"promo/internal/domain"
)

type workerPoolRepo struct {
	Repository
	mu        sync.Mutex
	remaining int
	delivered int
	complete  chan struct{}
}

func (r *workerPoolRepo) TakeMessage(context.Context, time.Time) (*domain.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.remaining == 0 {
		return nil, nil
	}
	r.remaining--
	return &domain.Message{ID: fmt.Sprintf("message-%d", r.remaining), UserID: int64(r.remaining + 1)}, nil
}

func (r *workerPoolRepo) FinishMessage(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delivered++
	if r.remaining == 0 && r.delivered == 100 {
		close(r.complete)
	}
	return nil
}

func (*workerPoolRepo) TakeInbox(context.Context, time.Time) (*domain.Event, error)  { return nil, nil }
func (*workerPoolRepo) DueDraws(context.Context, time.Time) ([]domain.Draw, error)   { return nil, nil }
func (*workerPoolRepo) DueOffers(context.Context, time.Time) ([]domain.Offer, error) { return nil, nil }

type gatedPoolMessenger struct {
	sendStub
	started chan struct{}
	release chan struct{}
}

func (m *gatedPoolMessenger) Send(ctx context.Context, _ domain.Message) error {
	select {
	case m.started <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-m.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestOutboxWorkerPoolBoundsConcurrencyAndDrainsBacklog(t *testing.T) {
	repo := &workerPoolRepo{remaining: 100, complete: make(chan struct{})}
	messenger := &gatedPoolMessenger{started: make(chan struct{}, 100), release: make(chan struct{})}
	service := New(repo, messenger, RealClock{}, domain.CryptoRandom{}, WithOutboxWorkers(2))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { service.Work(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("worker pool did not stop after cancellation")
		}
	})
	for range 2 {
		select {
		case <-messenger.started:
		case <-time.After(time.Second):
			t.Fatal("configured workers did not start independently")
		}
	}
	select {
	case <-messenger.started:
		t.Fatal("worker pool exceeded its configured concurrency")
	case <-time.After(20 * time.Millisecond):
	}
	close(messenger.release)
	select {
	case <-repo.complete:
	case <-time.After(time.Second):
		t.Fatal("worker pool polled between messages in a nonempty backlog")
	}
}

func TestOutboxWorkerCountIsBounded(t *testing.T) {
	for _, count := range []int{-1, 0, 65} {
		service := New(nil, nil, RealClock{}, domain.CryptoRandom{}, WithOutboxWorkers(count))
		if service.outboxWorkers != 8 {
			t.Fatalf("invalid worker count changed the default: %d", service.outboxWorkers)
		}
	}
	service := New(nil, nil, RealClock{}, domain.CryptoRandom{}, WithOutboxWorkers(64))
	if service.outboxWorkers != 64 {
		t.Fatal("valid bounded worker count was ignored")
	}
}
