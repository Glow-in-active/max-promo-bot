package app

import (
	"context"

	"promo/internal/domain"
)

func (s *Service) dispatch(ctx context.Context, tx Tx, execution *executionContext, reply *domain.Message) error {
	switch execution.command.Action {
	case "start":
		reply.Text = "Добро пожаловать в Бот розыгрышей 🎁"
		reply.Buttons = menu()
		reply.Banner = true
	case "help":
		reply.Text = "Получите промокод партнёра или подпишитесь на его канал и зарегистрируйтесь в розыгрыше. " +
			"Каждый активный день даёт один шанс на главный приз, максимум десять. " +
			"Промокод не обязателен для участия."
		reply.Buttons = menu()
	case "rules":
		reply.Text = execution.campaign.Rules
		reply.Buttons = menu()
	case "chances":
		return s.chances(ctx, tx, execution.campaign, execution.command.UserID, reply)
	case "promo":
		return s.issuePromo(ctx, tx, execution, reply)
	case "participate":
		return s.showParticipation(ctx, tx, execution, reply)
	case "check":
		return s.registerParticipation(ctx, tx, execution, reply)
	case "claim_begin":
		return s.beginClaim(ctx, tx, execution, reply)
	case "claim_name":
		return s.saveClaimName(ctx, tx, execution, reply)
	case "claim":
		return s.claimPrize(ctx, tx, execution, reply)
	}

	return nil
}

func menu() []domain.Button {
	return []domain.Button{
		{Text: "Получить промокод", Payload: "promo"},
		{Text: "Участвовать в розыгрыше", Payload: "participate"},
		{Text: "Как участвовать", Payload: "help"},
		{Text: "Мои шансы", Payload: "chances"},
		{Text: "Правила акции", Payload: "rules"},
	}
}
