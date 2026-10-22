package app

import (
	"context"
	"fmt"
	"strings"

	"promo/internal/domain"
)

func (s *Service) beginClaim(ctx context.Context, tx Tx, execution *executionContext, reply *domain.Message) error {
	cmd := execution.command
	if execution.offer.ID == "" {
		return domain.UserFail(domain.Invalid, "Prize offer is missing.", "Не указан приз.")
	}
	if err := tx.LockSlot(ctx, execution.offer.SlotID); err != nil {
		return err
	}
	offer, err := tx.Offer(ctx, execution.offer.ID)
	if err != nil {
		return err
	}
	execution.now = s.Now()
	if err := offer.CanClaim(cmd.UserID, "Имя Фамилия", true, execution.now); err != nil {
		if clearErr := clearPendingOffer(ctx, tx, cmd.UserID, offer.ID); clearErr != nil {
			return clearErr
		}
		return err
	}
	if offer.Status == "claimed" {
		reply.Text = "Получение приза уже подтверждено ✅"
		return clearPendingOffer(ctx, tx, cmd.UserID, offer.ID)
	}
	if offer.Status == "pending" {
		deadline := execution.now.Add(execution.draw.ClaimTTL)
		offer.SentAt, offer.Deadline, offer.Status = &execution.now, &deadline, "notified"
		if err := tx.SaveOffer(ctx, offer); err != nil {
			return err
		}
	}
	pendingID, name, err := tx.PendingClaim(ctx, cmd.UserID)
	if err != nil {
		return err
	}
	if pendingID == offer.ID && name != "" {
		consentPrompt(reply, offer.ID)
		return nil
	}
	if pendingID != offer.ID {
		if err := tx.SetPendingClaim(ctx, cmd.UserID, offer.ID, ""); err != nil {
			return err
		}
	}
	reply.Text = "Введите ваше имя и фамилию одним сообщением. Затем подтвердите согласие на передачу данных для получения приза."
	return nil
}

func (s *Service) saveClaimName(ctx context.Context, tx Tx, execution *executionContext, reply *domain.Message) error {
	cmd := execution.command
	offerID, _, err := tx.PendingClaim(ctx, cmd.UserID)
	if err != nil {
		return err
	}
	if offerID == "" || offerID != execution.offer.ID {
		reply.Text = "Выберите действие в меню."
		reply.Buttons = menu()
		return nil
	}
	if err := tx.LockSlot(ctx, execution.offer.SlotID); err != nil {
		return err
	}
	offer, err := tx.Offer(ctx, offerID)
	if err != nil {
		return err
	}
	execution.now = s.Now()
	if err := offer.CanClaim(cmd.UserID, "Имя Фамилия", true, execution.now); err != nil {
		if clearErr := clearPendingOffer(ctx, tx, cmd.UserID, offer.ID); clearErr != nil {
			return clearErr
		}
		return err
	}
	if offer.Status == "claimed" {
		reply.Text = "Получение приза уже подтверждено ✅"
		return clearPendingOffer(ctx, tx, cmd.UserID, offer.ID)
	}
	name := strings.TrimSpace(cmd.Name)
	if len(strings.Fields(name)) < 2 || len([]rune(name)) > 120 {
		return domain.UserFail(domain.Invalid, "A full name is required in a single message.", "Введите имя и фамилию одним сообщением.")
	}
	if err := tx.SetPendingClaim(ctx, cmd.UserID, offerID, name); err != nil {
		return err
	}
	consentPrompt(reply, offerID)
	return nil
}

func clearPendingOffer(ctx context.Context, tx Tx, userID int64, offerID string) error {
	pendingID, _, err := tx.PendingClaim(ctx, userID)
	if err != nil || pendingID != offerID {
		return err
	}
	return tx.ClearPendingClaim(ctx, userID)
}

func consentPrompt(reply *domain.Message, offerID string) {
	reply.Text = "Подтвердите согласие на передачу имени и фамилии партнёру или билетному оператору для получения приза."
	reply.Buttons = []domain.Button{{Text: "Согласен, получить приз", Payload: "claim:" + offerID}}
}

func (s *Service) claimPrize(ctx context.Context, tx Tx, execution *executionContext, reply *domain.Message) error {
	cmd := execution.command
	if execution.offer.ID == "" {
		return domain.UserFail(domain.Invalid, "Prize offer is missing.", "Не указан приз.")
	}
	if err := tx.LockSlot(ctx, execution.offer.SlotID); err != nil {
		return err
	}
	offer, err := tx.Offer(ctx, execution.offer.ID)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(cmd.Name)
	if name == "" {
		pendingID, pendingName, err := tx.PendingClaim(ctx, cmd.UserID)
		if err != nil {
			return err
		}
		if pendingID == offer.ID {
			name = pendingName
		}
	}
	if offer.Status == "claimed" && offer.UserID == cmd.UserID {
		reply.Text = "Получение приза уже подтверждено ✅"
		return clearPendingOffer(ctx, tx, cmd.UserID, offer.ID)
	}
	execution.now = s.Now()
	if err := offer.CanClaim(cmd.UserID, name, cmd.Consent, execution.now); err != nil {
		if fault, ok := domain.FaultOf(err); ok && fault.Code == domain.Expired {
			if clearErr := clearPendingOffer(ctx, tx, cmd.UserID, offer.ID); clearErr != nil {
				return clearErr
			}
		}
		return err
	}
	if offer.SentAt == nil {
		offer.SentAt = &execution.now
	}
	offer.Status = "claimed"
	offer.Name = name
	offer.ClaimedAt = &execution.now
	offer.ConsentAt = &execution.now
	if err := tx.SaveOffer(ctx, offer); err != nil {
		return err
	}
	if err := clearPendingOffer(ctx, tx, cmd.UserID, offer.ID); err != nil {
		return err
	}
	if err := tx.Audit(ctx, execution.campaign.ID, "claim", fmt.Sprint(cmd.UserID), execution.now); err != nil {
		return err
	}
	reply.Text = "Получение приза подтверждено ✅ Команда свяжется с вами через этот бот."
	return nil
}

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
		if _, err := tx.UserForUpdate(ctx, before.UserID, s.Now()); err != nil {
			return err
		}
		if err := tx.LockSlot(ctx, before.SlotID); err != nil {
			return err
		}
		o, err := tx.Offer(ctx, offerID)
		if err != nil {
			return err
		}
		now := s.Now()
		if !o.CanExpire(now) {
			return nil
		}
		o.Status = "expired"
		if err := tx.SaveOffer(ctx, o); err != nil {
			return err
		}
		if err := clearPendingOffer(ctx, tx, o.UserID, o.ID); err != nil {
			return err
		}
		offers, err := tx.Offers(ctx, d.ID)
		if err != nil {
			return err
		}
		used := map[int64]bool{}
		for _, offer := range offers {
			used[offer.UserID] = true
		}
		for _, user := range d.Selection.Reserve {
			if !used[user] {
				next := domain.Offer{ID: identifier("offer"), SlotID: o.SlotID, DrawID: d.ID, UserID: user, Status: "pending"}
				if err := tx.SaveOffer(ctx, next); err != nil {
					return err
				}
				if err := s.enqueuePrize(ctx, tx, d, next, now); err != nil {
					return err
				}
				return tx.Audit(ctx, d.CampaignID, "reserve:"+o.SlotID, "scheduler", now)
			}
		}
		return tx.Audit(ctx, d.CampaignID, "reserve_exhausted:"+o.SlotID, "scheduler", now)
	})
}
