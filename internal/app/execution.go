package app

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"promo/internal/domain"
	"promo/internal/observability"
)

// executionContext belongs to one transaction attempt. Mutation handlers refresh
// now after acquiring locks so their deadline checks use the current time.
type executionContext struct {
	command      domain.Command
	campaign     domain.Campaign
	day          domain.Day
	draw         domain.Draw
	offer        domain.Offer
	checkedDay   string
	subscription Subscription
	now          time.Time
}

func (s *Service) prepareCommand(cmd domain.Command) (domain.Command, error) {
	if err := domain.ValidateCommand(cmd); err != nil {
		return domain.Command{}, err
	}
	if cmd.ID == "" {
		cmd.ID = identifier("command")
	}

	// Internal commands and legacy inbox rows have no source timestamp.
	if cmd.OccurredAt.IsZero() {
		cmd.OccurredAt = s.Now()
	}

	return cmd, nil
}

func (s *Service) checkSubscription(ctx context.Context, cmd domain.Command) (string, Subscription, error) {
	if cmd.Action != "check" {
		return "", SubscriptionUnknown, nil
	}

	day, err := s.repo.CurrentDay(ctx, cmd.CampaignID, cmd.OccurredAt)
	if err != nil {
		if _, ok := domain.FaultOf(err); ok {
			return "", SubscriptionUnknown, nil
		}
		return "", SubscriptionUnknown, err
	}
	if day.CanAct(s.Now()) != nil {
		return "", SubscriptionUnknown, nil
	}

	subscription, err := s.messenger.Subscription(ctx, day.ChannelID, cmd.UserID)
	if err != nil {
		attrs := append(observability.ErrorAttrs(err),
			slog.String("event_ref", observability.Ref(cmd.ID)),
			slog.String("campaign_ref", observability.Ref(cmd.CampaignID)))
		s.logger.LogAttrs(ctx, slog.LevelWarn, "subscription check failed", attrs...)
		subscription = SubscriptionUnknown
	}

	return day.ID, subscription, nil
}

func (s *Service) loadExecution(ctx context.Context, tx Tx, cmd domain.Command) (executionContext, error) {
	campaign, err := tx.Campaign(ctx, cmd.CampaignID, false)
	if err != nil {
		return executionContext{}, err
	}

	execution := executionContext{
		command:  cmd,
		campaign: campaign,
		now:      s.Now(),
	}

	// Draw locks precede user locks in every writer to prevent lock inversion.
	if err := loadPartnerDay(ctx, tx, &execution); err != nil {
		return executionContext{}, err
	}
	if err := loadClaimOffer(ctx, tx, &execution); err != nil {
		return executionContext{}, err
	}
	if _, err := tx.UserForUpdate(ctx, cmd.UserID, execution.now); err != nil {
		return executionContext{}, err
	}

	execution.now = s.Now()
	if err := tx.TouchCampaignUser(ctx, campaign.ID, cmd.UserID, execution.now); err != nil {
		return executionContext{}, err
	}

	return execution, nil
}

func loadPartnerDay(ctx context.Context, tx Tx, execution *executionContext) error {
	cmd, campaign := execution.command, execution.campaign
	if cmd.Action != "promo" && cmd.Action != "participate" && cmd.Action != "check" {
		return nil
	}

	day, err := tx.DayAt(ctx, campaign.ID, cmd.OccurredAt)
	execution.day = day
	if err != nil {
		if _, ok := domain.FaultOf(err); ok {
			return nil
		}
		return err
	}
	if cmd.Action != "check" {
		return nil
	}

	execution.draw, err = tx.Draw(ctx, day.DrawID, false)
	return err
}

func loadClaimOffer(ctx context.Context, tx Tx, execution *executionContext) error {
	cmd := execution.command
	offerID := cmd.OfferID
	if cmd.Action == "claim_name" {
		pendingID, _, err := tx.PendingClaim(ctx, cmd.UserID)
		if err != nil {
			return err
		}
		offerID = pendingID
	}
	if !strings.HasPrefix(cmd.Action, "claim") || offerID == "" {
		return nil
	}

	offer, err := tx.Offer(ctx, offerID)
	if err != nil {
		return err
	}
	draw, err := tx.Draw(ctx, offer.DrawID, true)
	if err != nil {
		return err
	}
	if draw.CampaignID != execution.campaign.ID {
		return domain.UserFail(
			domain.Forbidden,
			"Prize offer belongs to another campaign.",
			"Приз относится к другой акции.",
		)
	}

	execution.offer = offer
	execution.draw = draw
	return nil
}

func (s *Service) finishExecution(ctx context.Context, tx Tx, execution *executionContext, reply *domain.Message) error {
	if err := s.dispatch(ctx, tx, execution, reply); err != nil {
		fault, ok := domain.FaultOf(err)
		if !ok {
			return err
		}

		reply.Text = fault.UserMessage
		if reply.Text == "" {
			reply.Text = "Не удалось выполнить действие. Попробуйте ещё раз чуть позже."
		}
		if len(reply.Buttons) == 0 {
			reply.Buttons = menu()
		}
	}

	if err := tx.Enqueue(ctx, *reply, "reply:"+execution.command.ID, execution.now); err != nil {
		return err
	}

	return tx.CompleteInbox(ctx, execution.command.ID)
}
