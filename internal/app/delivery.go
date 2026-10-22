package app

import (
	"context"
	"time"

	"promo/internal/domain"
)

func (s *Service) enqueuePrize(ctx context.Context, tx Tx, d domain.Draw, o domain.Offer, now time.Time) error {
	message := domain.Message{
		ID:         identifier("message"),
		CampaignID: d.CampaignID,
		UserID:     o.UserID,
		OfferID:    o.ID,
		Text:       domain.PrizeText(d),
		Buttons:    []domain.Button{{Text: "Получить приз", Payload: "claim_begin:" + o.ID}},
	}
	return tx.Enqueue(ctx, message, "prize:"+o.ID, now)
}

func (s *Service) MarkDelivered(ctx context.Context, offerID string) error {
	return s.ConfirmDelivery(ctx, offerID, "sender")
}

// ConfirmDelivery also supports an audited operator acknowledgement after an
// ambiguous MAX response. It never extends an existing claim deadline.
func (s *Service) ConfirmDelivery(ctx context.Context, offerID, actor string) error {
	before, err := s.repo.Offer(ctx, offerID)
	if err != nil {
		return err
	}
	draw, err := s.repo.Draw(ctx, before.DrawID)
	if err != nil {
		return err
	}
	return s.repo.Within(ctx, func(tx Tx) error {
		if _, err := tx.Campaign(ctx, draw.CampaignID, false); err != nil {
			return err
		}
		d, err := tx.Draw(ctx, draw.ID, true)
		if err != nil {
			return err
		}
		if d.State != domain.DrawApproved {
			return domain.UserFail(domain.InvalidTransition, "Draw result has not been approved.", "Результат ещё не подтверждён.")
		}
		if err := tx.LockSlot(ctx, before.SlotID); err != nil {
			return err
		}
		o, err := tx.Offer(ctx, offerID)
		if err != nil {
			return err
		}
		if o.Status != "pending" {
			return nil
		}
		now := s.Now()
		deadline := now.Add(d.ClaimTTL)
		o.SentAt = &now
		o.Deadline = &deadline
		o.Status = "notified"
		if err := tx.SaveOffer(ctx, o); err != nil {
			return err
		}
		return tx.Audit(ctx, draw.CampaignID, "delivery_confirmed:"+offerID, actor, now)
	})
}

func (s *Service) ListOffers(ctx context.Context, campaignID, drawID string) ([]domain.Offer, error) {
	var offers []domain.Offer
	err := s.repo.Within(ctx, func(tx Tx) error {
		if _, err := tx.Campaign(ctx, campaignID, false); err != nil {
			return err
		}
		d, err := tx.Draw(ctx, drawID, false)
		if err != nil {
			return err
		}
		if d.CampaignID != campaignID {
			return domain.UserFail(domain.Forbidden, "Draw belongs to another campaign.", "Розыгрыш относится к другой акции.")
		}
		offers, err = tx.Offers(ctx, drawID)
		return err
	})
	return offers, err
}

func (s *Service) RetryDelivery(ctx context.Context, offerID, actor string) error {
	before, err := s.repo.Offer(ctx, offerID)
	if err != nil {
		return err
	}
	draw, err := s.repo.Draw(ctx, before.DrawID)
	if err != nil {
		return err
	}
	return s.repo.Within(ctx, func(tx Tx) error {
		if _, err := tx.Campaign(ctx, draw.CampaignID, false); err != nil {
			return err
		}
		d, err := tx.Draw(ctx, draw.ID, true)
		if err != nil {
			return err
		}
		if d.State != domain.DrawApproved {
			return domain.UserFail(domain.InvalidTransition, "Draw result has not been approved.", "Результат ещё не подтверждён.")
		}
		if err := tx.LockSlot(ctx, before.SlotID); err != nil {
			return err
		}
		o, err := tx.Offer(ctx, offerID)
		if err != nil {
			return err
		}
		if o.Status != "pending" {
			return domain.UserFail(
				domain.InvalidTransition,
				"Delivery retry is allowed only before delivery confirmation.",
				"Повторная отправка разрешена только до подтверждения доставки.",
			)
		}
		now := s.Now()
		if err := tx.RetryOfferMessage(ctx, offerID, now); err != nil {
			return err
		}
		return tx.Audit(ctx, draw.CampaignID, "delivery_retry:"+offerID, actor, now)
	})
}
