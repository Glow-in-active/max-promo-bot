package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"promo/internal/config"
	"promo/internal/observability"
	"promo/internal/storage/postgres"
)

type runtime struct {
	config config.Config
	logger *slog.Logger
	repo   *postgres.Store
}

func run(args []string) error {
	command, err := parseCommand(args)
	if err != nil {
		return err
	}
	if command.name == "healthcheck" {
		return runHealthcheck()
	}

	c, err := config.Load()
	if err != nil {
		return err
	}
	if command.name == "serve" {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	logger, err := observability.New(os.Stdout, c.LogLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repo, err := connectDatabase(ctx, c.DatabaseURL)
	if err != nil {
		return err
	}
	defer repo.Close()

	runtime := runtime{config: c, logger: logger, repo: repo}
	return runtime.execute(ctx, stop, command)
}

func connectDatabase(ctx context.Context, databaseURL string) (*postgres.Store, error) {
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	return postgres.New(connectCtx, databaseURL)
}

func (r runtime) execute(ctx context.Context, stop context.CancelFunc, command command) error {
	startupCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	switch command.name {
	case "migrate":
		return r.migrate(ctx, startupCtx)
	case "setup":
		return r.setup(ctx, startupCtx, command.campaign)
	}

	platform, err := r.preparePlatform(startupCtx)
	if err != nil {
		return err
	}
	cancel()

	return r.serve(ctx, stop, platform)
}

func (r runtime) preparePlatform(ctx context.Context) (botPlatform, error) {
	if r.config.Mode == "production" {
		if _, err := r.repo.Campaign(ctx, r.config.CampaignID); err != nil {
			return botPlatform{}, fmt.Errorf("load configured campaign (run setup first): %w", err)
		}
	}

	return newBotPlatform(r.config)
}
