package app

import (
	"context"

	"promo/internal/domain"
)

func (s *Service) issuePromo(ctx context.Context, tx Tx, execution *executionContext, reply *domain.Message) error {
	cmd, campaign, day := execution.command, execution.campaign, execution.day
	if err := tx.RecordPromoRequest(ctx, campaign.ID, cmd.UserID, execution.now); err != nil {
		return err
	}
	if day.ID == "" {
		return domain.UserFail(domain.NotActive, "No partner day is active for this event.", "Сегодня нет активного партнёрского дня.")
	}
	assigned, err := tx.Assignment(ctx, day.ID, cmd.UserID)
	if err != nil {
		return err
	}
	if assigned != nil {
		reply.Text = domain.PromoText(*assigned, day)
		return nil
	}
	if err := campaign.CanAct(s.Now()); err != nil {
		return err
	}
	if err := tx.LockPool(ctx, day.ID); err != nil {
		return err
	}
	execution.now = s.Now()
	if err := campaign.CanAct(execution.now); err != nil {
		return err
	}
	if err := day.CanAct(execution.now); err != nil {
		return err
	}
	code, err := tx.NextPromo(ctx, day.ID)
	if err != nil {
		return err
	}
	if code == nil {
		reply.Buttons = []domain.Button{{Text: "Участвовать в розыгрыше", Payload: "participate"}}
		return domain.UserFail(
			domain.CodesExhausted,
			"Partner promo code pool is exhausted.",
			"Промокоды партнёра закончились. Вы всё ещё можете подписаться и участвовать в розыгрыше.",
		)
	}
	active, err := tx.Activities(ctx, campaign.ID, cmd.UserID)
	if err != nil {
		return err
	}
	grant, err := domain.GrantChance(active, day)
	if err != nil {
		return err
	}
	if err := tx.AssignPromo(ctx, code.ID, cmd.UserID, execution.now); err != nil {
		return err
	}
	if grant {
		if err := tx.AddActivity(ctx, campaign.ID, cmd.UserID, day, "promo", execution.now); err != nil {
			return err
		}
	}
	reply.Text = domain.PromoText(*code, day)
	reply.Buttons = []domain.Button{{Text: "Участвовать в розыгрыше", Payload: "participate"}, {Text: "Мои шансы", Payload: "chances"}}
	return nil
}
