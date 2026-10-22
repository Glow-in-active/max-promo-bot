package app

import (
	"context"
	"crypto/rand"
	"log/slog"
	"time"

	"promo/internal/domain"
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
	service := &Service{
		repo:          repo,
		messenger:     messenger,
		clock:         clock,
		random:        random,
		logger:        slog.Default(),
		workTimeout:   15 * time.Second,
		outboxWorkers: 8,
	}

	for _, option := range options {
		option(service)
	}

	return service
}

func (s *Service) Now() time.Time {
	return s.clock.Now().UTC()
}

func (s *Service) Execute(ctx context.Context, cmd domain.Command) (domain.Message, error) {
	cmd, err := s.prepareCommand(cmd)
	if err != nil {
		return domain.Message{}, err
	}

	// Deadlines are checked again under transaction locks.
	checkedDay, subscription, err := s.checkSubscription(ctx, cmd)
	if err != nil {
		return domain.Message{}, err
	}

	var reply domain.Message
	err = s.repo.Within(ctx, func(tx Tx) error {
		reply = domain.Message{
			ID:         identifier("message"),
			CampaignID: cmd.CampaignID,
			UserID:     cmd.UserID,
		}

		done, err := tx.InboxDone(ctx, cmd.ID)
		if err != nil {
			return err
		}
		if done {
			return nil
		}

		execution, err := s.loadExecution(ctx, tx, cmd)
		if err != nil {
			return err
		}
		execution.checkedDay = checkedDay
		execution.subscription = subscription

		return s.finishExecution(ctx, tx, &execution, &reply)
	})

	return reply, err
}

func identifier(prefix string) string {
	return prefix + "_" + rand.Text()
}
