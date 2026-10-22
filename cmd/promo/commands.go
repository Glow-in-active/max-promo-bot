package main

import (
	"context"
	"log/slog"

	"promo/internal/config"
	"promo/internal/domain"
	"promo/internal/healthcheck"
	"promo/internal/observability"
)

type command struct {
	name     string
	campaign domain.Setup
}

func parseCommand(args []string) (command, error) {
	parsed := command{name: "serve"}
	if len(args) > 0 {
		parsed.name = args[0]
	}

	switch parsed.name {
	case "healthcheck":
		if len(args) != 1 {
			return command{}, usageError{}
		}
	case "serve", "migrate":
		if len(args) > 1 {
			return command{}, usageError{}
		}
	case "setup":
		if len(args) != 2 {
			return command{}, usageError{}
		}
		campaign, err := config.LoadCampaign(args[1])
		if err != nil {
			return command{}, err
		}
		parsed.campaign = campaign
	default:
		return command{}, usageError{}
	}

	return parsed, nil
}

func commandName(args []string) string {
	if len(args) == 0 {
		return "serve"
	}

	switch args[0] {
	case "serve", "migrate", "setup", "healthcheck":
		return args[0]
	default:
		return "unknown"
	}
}

func runHealthcheck() error {
	address, err := config.HTTPAddress()
	if err != nil {
		return err
	}
	return healthcheck.Check(context.Background(), address)
}

func (r runtime) migrate(ctx, startupCtx context.Context) error {
	if err := r.repo.Migrate(startupCtx); err != nil {
		return err
	}

	r.logger.InfoContext(ctx, "database migrations completed")
	return nil
}

func (r runtime) setup(ctx, startupCtx context.Context, campaign domain.Setup) error {
	if err := r.repo.Setup(startupCtx, campaign); err != nil {
		return err
	}

	r.logger.InfoContext(ctx, "campaign imported", "campaign_ref", observability.Ref(campaign.Campaign.ID))
	return nil
}

type usageError struct{}

func (usageError) Error() string {
	return "usage: promo [serve|migrate|setup campaign.json|healthcheck]"
}

func (e usageError) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("error_code", "invalid_arguments"),
		slog.String("reason", e.Error()),
	}
}
