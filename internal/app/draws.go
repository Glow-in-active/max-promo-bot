package app

import (
	"context"

	"promo/internal/domain"
	"promo/internal/observability"
)

func (s *Service) RunDraw(ctx context.Context, campaignID, drawID, actor string) (domain.Draw, error) {
	var result domain.Draw
	created := false
	initial, err := s.repo.Draw(ctx, drawID)
	if err != nil {
		return result, err
	}
	err = s.repo.Within(ctx, func(tx Tx) error {
		created = false
		// The exclusive campaign lock freezes both code and registration activity for the final.
		c, err := tx.Campaign(ctx, campaignID, initial.Kind == "final")
		if err != nil {
			return err
		}
		d, err := tx.Draw(ctx, drawID, true)
		if err != nil {
			return err
		}
		if d.CampaignID != c.ID {
			return domain.UserFail(domain.Forbidden, "Draw belongs to another campaign.", "Розыгрыш относится к другой акции.")
		}
		if d.State != domain.DrawOpen {
			result = d
			return nil
		}
		now := s.Now()
		if err := d.CanDraw(now); err != nil {
			return err
		}
		candidates, err := tx.Candidates(ctx, d)
		if err != nil {
			return err
		}
		selection, err := domain.Select(candidates, d.WinnerCount, d.ReserveCount, s.random)
		if err != nil {
			return err
		}
		d.Candidates = candidates
		d.Selection = selection
		d.State = domain.DrawDrawn
		d.DrawnAt = &now
		if d.Kind == "final" {
			if err := tx.SetCampaignClosed(ctx, c.ID); err != nil {
				return err
			}
		}
		if err := tx.SaveDraw(ctx, d); err != nil {
			return err
		}
		if err := tx.Audit(ctx, c.ID, "draw:"+d.ID, actor, now); err != nil {
			return err
		}
		result = d
		created = true
		return nil
	})
	if err == nil && created {
		s.logger.InfoContext(ctx, "draw completed", "draw_ref", observability.Ref(result.ID), "kind", result.Kind, "candidates", len(result.Candidates), "winners", len(result.Selection.Winners))
	}
	return result, err
}

func (s *Service) ApproveDraw(ctx context.Context, campaignID, drawID, actor string) (domain.Draw, error) {
	var result domain.Draw
	approved := false
	err := s.repo.Within(ctx, func(tx Tx) error {
		approved = false
		if _, err := tx.Campaign(ctx, campaignID, false); err != nil {
			return err
		}
		d, err := tx.Draw(ctx, drawID, true)
		if err != nil {
			return err
		}
		if d.CampaignID != campaignID {
			return domain.UserFail(domain.Forbidden, "Draw belongs to another campaign.", "Розыгрыш относится к другой акции.")
		}
		if d.State == domain.DrawApproved {
			result = d
			return nil
		}
		if err := d.CanApprove(); err != nil {
			return err
		}
		now := s.Now()
		d.State = domain.DrawApproved
		d.ApprovedAt = &now
		if err := tx.SaveDraw(ctx, d); err != nil {
			return err
		}
		for i, user := range d.Selection.Winners {
			slotID := identifier("slot")
			if err := tx.CreateSlot(ctx, slotID, d.ID, i+1); err != nil {
				return err
			}
			offer := domain.Offer{ID: identifier("offer"), SlotID: slotID, DrawID: d.ID, UserID: user, Status: "pending"}
			if err := tx.SaveOffer(ctx, offer); err != nil {
				return err
			}
			if err := s.enqueuePrize(ctx, tx, d, offer, now); err != nil {
				return err
			}
		}
		if err := tx.Audit(ctx, campaignID, "approve:"+d.ID, actor, now); err != nil {
			return err
		}
		result = d
		approved = true
		return nil
	})
	if err == nil && approved {
		s.logger.InfoContext(ctx, "draw approved", "draw_ref", observability.Ref(result.ID), "winners", len(result.Selection.Winners))
	}
	return result, err
}
