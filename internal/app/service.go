package app

import (
	"context"
	"crypto/rand"
	"log/slog"
	"strings"
	"time"

	"promo/internal/domain"
	"promo/internal/observability"
)

type Service struct {
	repo          Repository
	messenger     Messenger
	clock         Clock
	random        domain.Random
	logger        *slog.Logger
	workTimeout   time.Duration
	outboxWorkers int
}

func New(repo Repository, messenger Messenger, clock Clock, random domain.Random, options ...Option) *Service {
	service := &Service{repo: repo, messenger: messenger, clock: clock, random: random, logger: slog.Default(), workTimeout: 15 * time.Second, outboxWorkers: 8}
	for _, option := range options {
		option(service)
	}
	return service
}

func (s *Service) Now() time.Time { return s.clock.Now().UTC() }

func identifier(prefix string) string { return prefix + "_" + rand.Text() }

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

func (s *Service) Execute(ctx context.Context, cmd domain.Command) (domain.Message, error) {
	if err := domain.ValidateCommand(cmd); err != nil {
		return domain.Message{}, err
	}
	if cmd.ID == "" {
		cmd.ID = identifier("command")
	}

	// Internal commands and legacy inbox rows have no source timestamp.
	if cmd.OccurredAt.IsZero() {
		cmd.OccurredAt = s.Now()
	}
	// The remote check precedes the transaction; its answer is not a day/registration lock.
	var checkedDay string
	var subscription Subscription
	if cmd.Action == "check" {
		day, err := s.repo.CurrentDay(ctx, cmd.CampaignID, cmd.OccurredAt)
		if err != nil {
			if _, ok := domain.FaultOf(err); !ok {
				return domain.Message{}, err
			}
		} else if day.CanAct(s.Now()) == nil {
			checkedDay = day.ID
			subscription, err = s.messenger.Subscription(ctx, day.ChannelID, cmd.UserID)
			if err != nil {
				attrs := append(observability.ErrorAttrs(err),
					slog.String("event_ref", observability.Ref(cmd.ID)),
					slog.String("campaign_ref", observability.Ref(cmd.CampaignID)))
				s.logger.LogAttrs(ctx, slog.LevelWarn, "subscription check failed", attrs...)
				subscription = SubscriptionUnknown
			}
		}
	}
	var reply domain.Message
	err := s.repo.Within(ctx, func(tx Tx) error {
		reply = domain.Message{ID: identifier("message"), CampaignID: cmd.CampaignID, UserID: cmd.UserID}
		done, err := tx.InboxDone(ctx, cmd.ID)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		campaign, err := tx.Campaign(ctx, cmd.CampaignID, false)
		if err != nil {
			return err
		}
		// Draw locks precede user locks in every writer to prevent lock inversion.
		var day domain.Day
		var draw domain.Draw
		now := s.Now()
		if cmd.Action == "promo" || cmd.Action == "participate" || cmd.Action == "check" {
			day, err = tx.DayAt(ctx, campaign.ID, cmd.OccurredAt)
			if err != nil {
				if _, ok := domain.FaultOf(err); !ok {
					return err
				}
			}
			if err == nil && cmd.Action == "check" {
				draw, err = tx.Draw(ctx, day.DrawID, false)
				if err != nil {
					return err
				}
			}
		}
		var claimOffer domain.Offer
		claimOfferID := cmd.OfferID
		if cmd.Action == "claim_name" {
			claimOfferID, _, err = tx.PendingClaim(ctx, cmd.UserID)
			if err != nil {
				return err
			}
		}
		if strings.HasPrefix(cmd.Action, "claim") && claimOfferID != "" {
			claimOffer, err = tx.Offer(ctx, claimOfferID)
			if err != nil {
				return err
			}
			draw, err = tx.Draw(ctx, claimOffer.DrawID, true)
			if err != nil {
				return err
			}
			if draw.CampaignID != campaign.ID {
				return domain.UserFail(domain.Forbidden, "Prize offer belongs to another campaign.", "Приз относится к другой акции.")
			}
		}
		if _, err = tx.UserForUpdate(ctx, cmd.UserID, now); err != nil {
			return err
		}
		now = s.Now()
		if err = tx.TouchCampaignUser(ctx, campaign.ID, cmd.UserID, now); err != nil {
			return err
		}
		execution := executionContext{
			command: cmd, campaign: campaign, day: day, draw: draw, offer: claimOffer,
			checkedDay: checkedDay, subscription: subscription, now: now,
		}
		resultErr := s.dispatch(ctx, tx, &execution, &reply)
		if resultErr != nil {
			if f, ok := domain.FaultOf(resultErr); ok {
				reply.Text = f.UserMessage
				if reply.Text == "" {
					reply.Text = "Не удалось выполнить действие. Попробуйте ещё раз чуть позже."
				}
				if len(reply.Buttons) == 0 {
					reply.Buttons = menu()
				}
			} else {
				return resultErr
			}
		}
		if err := tx.Enqueue(ctx, reply, "reply:"+cmd.ID, execution.now); err != nil {
			return err
		}
		return tx.CompleteInbox(ctx, cmd.ID)
	})
	return reply, err
}

func (s *Service) dispatch(ctx context.Context, tx Tx, execution *executionContext, reply *domain.Message) error {
	switch execution.command.Action {
	case "start":
		reply.Text = "Добро пожаловать в Бот розыгрышей 🎁"
		reply.Buttons = menu()
		reply.Banner = true
	case "help":
		reply.Text = "Получите промокод партнёра или подпишитесь на его канал и зарегистрируйтесь в розыгрыше. Каждый активный день даёт один шанс на главный приз, максимум десять. Промокод не обязателен для участия."
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
