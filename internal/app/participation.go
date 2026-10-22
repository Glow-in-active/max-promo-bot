package app

import (
	"context"
	"fmt"
	"strings"

	"promo/internal/domain"
)

func (s *Service) showParticipation(ctx context.Context, tx Tx, execution *executionContext, reply *domain.Message) error {
	campaign, day := execution.campaign, execution.day
	execution.now = s.Now()
	if err := campaign.CanAct(execution.now); err != nil {
		return err
	}
	if day.ID == "" {
		return domain.UserFail(domain.NotActive, "No partner day is active for this event.", "Сегодня нет активного партнёрского дня.")
	}
	if err := day.CanAct(execution.now); err != nil {
		return err
	}
	reply.Text = "Чтобы участвовать в розыгрыше от «" + day.Partner + "», подпишитесь на канал партнёра и вернитесь проверить подписку."
	reply.Buttons = subscriptionButtons(day)
	return nil
}

func (s *Service) registerParticipation(ctx context.Context, tx Tx, execution *executionContext, reply *domain.Message) error {
	cmd, campaign, day, draw := execution.command, execution.campaign, execution.day, execution.draw
	if day.ID == "" {
		return domain.UserFail(domain.NotActive, "No partner day is active for this event.", "Сегодня нет активного партнёрского дня.")
	}
	exists, err := tx.HasEntry(ctx, day.DrawID, cmd.UserID)
	if err != nil {
		return err
	}
	if exists {
		reply.Text = "Вы уже участвуете в сегодняшнем розыгрыше ✅"
		return s.chances(ctx, tx, campaign, cmd.UserID, reply)
	}
	execution.now = s.Now()
	if err := campaign.CanAct(execution.now); err != nil {
		return err
	}
	if err := draw.CanRegister(execution.now); err != nil {
		return err
	}
	if err := day.CanAct(execution.now); err != nil {
		return err
	}
	reply.Buttons = subscriptionButtons(day)
	if execution.checkedDay != day.ID || execution.subscription == SubscriptionUnknown {
		return domain.UserFail(domain.SubscriptionUnavailable, "Subscription check is temporarily unavailable.", "Не удалось проверить подписку. Попробуйте ещё раз чуть позже.")
	}
	if execution.subscription != SubscriptionYes {
		return domain.UserFail(
			domain.NotSubscribed,
			"Participant is not subscribed to the partner channel.",
			"Пока не вижу подписку. Подпишитесь на канал партнёра и проверьте ещё раз.",
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
	if err := tx.AddEntry(ctx, draw.ID, cmd.UserID, execution.now); err != nil {
		return err
	}
	if grant {
		if err := tx.AddActivity(ctx, campaign.ID, cmd.UserID, day, "entry", execution.now); err != nil {
			return err
		}
	}
	reply.Text = "Вы в лайнапе ✅ Заявка зарегистрирована. Возвращайтесь завтра к новому партнёру."
	return s.chances(ctx, tx, campaign, cmd.UserID, reply)
}

func subscriptionButtons(day domain.Day) []domain.Button {
	return []domain.Button{{Text: "Подписаться на " + day.Partner, URL: day.PartnerURL}, {Text: "Проверить подписку", Payload: "check"}}
}

func (s *Service) chances(ctx context.Context, tx Tx, c domain.Campaign, user int64, reply *domain.Message) error {
	active, err := tx.Activities(ctx, c.ID, user)
	if err != nil {
		return err
	}
	days, err := tx.Days(ctx, c.ID)
	if err != nil {
		return err
	}
	remaining := 0
	for _, day := range days {
		if s.Now().Before(day.StartsAt) {
			remaining++
		}
	}
	var visited []string
	for _, a := range active {
		visited = append(visited, fmt.Sprintf("день %d: %s", a.DayNumber, a.Partner))
	}
	if reply.Text != "" {
		reply.Text += "\n\n"
	}
	reply.Text += fmt.Sprintf(
		"Ваш фестивальный пропуск\nНакоплено шансов: %d из 10\n"+
			"Пройденные дни: %s\nОсталось дней: %d\nГлавный приз: %s",
		len(active), strings.Join(visited, ", "), remaining, c.MainPrize,
	)
	reply.Buttons = menu()
	return nil
}
