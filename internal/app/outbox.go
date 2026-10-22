package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"promo/internal/domain"
	"promo/internal/observability"
)

func (s *Service) processMessage(ctx context.Context) error {
	_, err := s.processNextMessage(ctx)
	return err
}

func (s *Service) processNextMessage(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.workTimeout)
	defer cancel()

	message, err := s.repo.TakeMessage(ctx, s.Now())
	if err != nil || message == nil {
		return false, err
	}
	s.logger.DebugContext(ctx, "outbox message acquired",
		"message_ref", observability.Ref(message.ID),
		"attempt", message.Attempts)

	return true, s.deliverMessage(ctx, *message)
}

func (s *Service) deliverMessage(ctx context.Context, message domain.Message) error {
	sendCtx := ctx
	cancelSend := func() {}
	if message.OfferID != "" {
		offer, err := s.repo.Offer(ctx, message.OfferID)
		if err != nil {
			return err
		}
		if offer.Status == "expired" || offer.Status == "claimed" || offer.Deadline != nil && !s.Now().Before(*offer.Deadline) {
			return s.repo.FinishMessage(ctx, message.ID)
		}
		if offer.Deadline != nil {
			sendCtx, cancelSend = context.WithTimeout(ctx, offer.Deadline.Sub(s.Now()))
		}
	}
	defer cancelSend()

	if err := s.messenger.Send(sendCtx, message); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(sendCtx.Err(), context.DeadlineExceeded) {
			return s.repo.FinishMessage(ctx, message.ID)
		}
		return s.recordDeliveryFailure(ctx, message, err)
	}
	return s.completeDelivery(ctx, message)
}

func (s *Service) recordDeliveryFailure(ctx context.Context, message domain.Message, err error) error {
	permanent := message.Attempts >= 5
	detail := "delivery failed"
	var uncertain interface{ Uncertain() bool }
	var temporary interface{ Temporary() bool }
	if message.OfferID != "" && errors.As(err, &uncertain) && uncertain.Uncertain() {
		permanent = true
		detail = "delivery uncertain"
	} else if errors.As(err, &temporary) && !temporary.Temporary() {
		permanent = true
		detail = "delivery rejected"
	}

	delay := backoff(message.Attempts)
	var provider interface{ RetryDelay() time.Duration }
	if errors.As(err, &provider) {
		delay = max(delay, provider.RetryDelay())
	}
	next := s.Now().Add(delay)
	if retryErr := s.repo.RetryMessage(ctx, message.ID, detail, next, permanent); retryErr != nil {
		return retryErr
	}

	attrs := append(observability.ErrorAttrs(err),
		slog.String("message_ref", observability.Ref(message.ID)),
		slog.Int("attempt", message.Attempts),
		slog.Bool("failed", permanent))
	level := slog.LevelWarn
	if permanent {
		level = slog.LevelError
	} else {
		attrs = append(attrs, slog.Time("retry_at", next))
	}
	s.logger.LogAttrs(ctx, level, detail, attrs...)
	return nil
}

func (s *Service) completeDelivery(ctx context.Context, message domain.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Persist the claim deadline before acknowledging the delivery job.
	if message.OfferID != "" {
		if err := s.MarkDelivered(ctx, message.OfferID); err != nil {
			return err
		}
	}
	if err := s.repo.FinishMessage(ctx, message.ID); err != nil {
		return err
	}
	s.logger.DebugContext(ctx, "outbox message delivered", "message_ref", observability.Ref(message.ID))
	return nil
}
