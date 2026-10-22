package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"promo/internal/observability"
)

func WithWorkTimeout(timeout time.Duration) Option {
	return func(service *Service) {
		if timeout > 0 && timeout < 30*time.Second {
			service.workTimeout = timeout
		}
	}
}

func WithOutboxWorkers(count int) Option {
	return func(service *Service) {
		if count > 0 && count <= 64 {
			service.outboxWorkers = count
		}
	}
}

func (s *Service) Tick(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var failures []error
	queryCtx, cancel := context.WithTimeout(ctx, s.workTimeout)
	draws, err := s.repo.DueDraws(queryCtx, s.Now())
	cancel()
	failures = append(failures, err)
	for _, d := range draws {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		jobCtx, cancel := context.WithTimeout(ctx, s.workTimeout)
		_, err := s.RunDraw(jobCtx, d.CampaignID, d.ID, "scheduler")
		cancel()
		failures = append(failures, err)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(append(failures, err)...)
	}
	queryCtx, cancel = context.WithTimeout(ctx, s.workTimeout)
	offers, err := s.repo.DueOffers(queryCtx, s.Now())
	cancel()
	failures = append(failures, err)
	for _, o := range offers {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		jobCtx, cancel := context.WithTimeout(ctx, s.workTimeout)
		err := s.ExpireOffer(jobCtx, o.ID)
		cancel()
		failures = append(failures, err)
	}
	return errors.Join(append(failures, ctx.Err())...)
}

func (s *Service) Work(ctx context.Context, logger *slog.Logger) {
	loops := []struct {
		name string
		run  func(context.Context) error
	}{
		{name: "inbox", run: s.processInbox},
		{name: "scheduler", run: s.Tick},
	}
	logger.InfoContext(ctx, "workers started")
	done := make(chan struct{}, len(loops)+s.outboxWorkers)
	for _, loop := range loops {
		go func() {
			defer func() { done <- struct{}{} }()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				if err := loop.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					attrs := append(observability.ErrorAttrs(err), slog.String("worker", loop.name))
					logger.LogAttrs(ctx, slog.LevelError, "background task failed", attrs...)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	for range s.outboxWorkers {
		go func() {
			defer func() { done <- struct{}{} }()
			for ctx.Err() == nil {
				processed, err := s.processNextMessage(ctx)
				if err != nil && !errors.Is(err, context.Canceled) {
					attrs := append(observability.ErrorAttrs(err), slog.String("worker", "outbox"))
					logger.LogAttrs(ctx, slog.LevelError, "background task failed", attrs...)
				}
				if processed && err == nil {
					continue
				}
				timer := time.NewTimer(100 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
	for range len(loops) + s.outboxWorkers {
		<-done
	}
	logger.InfoContext(ctx, "workers stopped")
}

func (s *Service) processInbox(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.workTimeout)
	defer cancel()
	event, err := s.repo.TakeInbox(ctx, s.Now())
	if err != nil || event == nil {
		return err
	}
	s.logger.DebugContext(ctx, "inbox event acquired",
		"event_ref", observability.Ref(event.ID),
		"attempt", event.Attempts)
	_, err = s.Execute(ctx, event.Command)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failed := event.Attempts >= 5
		next := s.Now().Add(backoff(event.Attempts))
		attrs := append(observability.ErrorAttrs(err),
			slog.String("event_ref", observability.Ref(event.ID)),
			slog.Int("attempt", event.Attempts),
			slog.Bool("failed", failed))
		level := slog.LevelWarn
		if failed {
			level = slog.LevelError
		} else {
			attrs = append(attrs, slog.Time("retry_at", next))
		}
		if err := s.repo.RetryInbox(ctx, event.ID, "processing failed", next, failed); err != nil {
			return err
		}
		s.logger.LogAttrs(ctx, level, "inbox processing failed", attrs...)
		return nil
	}
	s.logger.DebugContext(ctx, "inbox event completed", "event_ref", observability.Ref(event.ID))
	return nil
}

func backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Second * time.Duration(1<<attempt)
}
