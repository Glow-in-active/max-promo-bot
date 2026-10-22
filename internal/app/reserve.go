package app

import (
	"context"
	"time"

	"promo/internal/domain"
)

func (s *Service) ExpireOffer(ctx context.Context, offerID string) error {
	before, err := s.repo.Offer(ctx, offerID)
	if err != nil {
		return err
	}
	draw, err := s.repo.Draw(ctx, before.DrawID)
	if err != nil {
		return err
	}
	return s.repo.Within(ctx, func(tx Tx) error {
		return s.expireOffer(ctx, tx, draw, before)
	})
}

func (s *Service) expireOffer(ctx context.Context, tx Tx, draw domain.Draw, before domain.Offer) error {
	if _, err := tx.Campaign(ctx, draw.CampaignID, false); err != nil {
		return err
	}
	draw, err := tx.Draw(ctx, draw.ID, true)
	if err != nil {
		return err
	}
	if draw.State != domain.DrawApproved {
		return domain.UserFail(domain.InvalidTransition, "Draw result has not been approved.", "Результат ещё не подтверждён.")
	}
	if _, err := tx.UserForUpdate(ctx, before.UserID, s.Now()); err != nil {
		return err
	}
	if err := tx.LockSlot(ctx, before.SlotID); err != nil {
		return err
	}
	offer, err := tx.Offer(ctx, before.ID)
	if err != nil {
		return err
	}
	now := s.Now()
	if !offer.CanExpire(now) {
		return nil
	}
	offer.Status = "expired"
	if err := tx.SaveOffer(ctx, offer); err != nil {
		return err
	}
	if err := clearPendingOffer(ctx, tx, offer.UserID, offer.ID); err != nil {
		return err
	}
	return s.offerReservePrize(ctx, tx, draw, offer.SlotID, now)
}

func (s *Service) offerReservePrize(ctx context.Context, tx Tx, draw domain.Draw, slotID string, now time.Time) error {
	offers, err := tx.Offers(ctx, draw.ID)
	if err != nil {
		return err
	}
	used := make(map[int64]bool, len(offers))
	for _, offer := range offers {
		used[offer.UserID] = true
	}
	for _, userID := range draw.Selection.Reserve {
		if used[userID] {
			continue
		}
		next := domain.Offer{
			ID:     identifier("offer"),
			SlotID: slotID,
			DrawID: draw.ID,
			UserID: userID,
			Status: "pending",
		}
		if err := tx.SaveOffer(ctx, next); err != nil {
			return err
		}
		if err := s.enqueuePrize(ctx, tx, draw, next, now); err != nil {
			return err
		}
		return tx.Audit(ctx, draw.CampaignID, "reserve:"+slotID, "scheduler", now)
	}
	return tx.Audit(ctx, draw.CampaignID, "reserve_exhausted:"+slotID, "scheduler", now)
}
