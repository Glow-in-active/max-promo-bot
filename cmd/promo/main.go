package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"promo/internal/app"
	"promo/internal/config"
	"promo/internal/domain"
	"promo/internal/healthcheck"
	"promo/internal/observability"
	"promo/internal/platform/demo"
	"promo/internal/platform/maxapi"
	"promo/internal/storage/postgres"
	"promo/internal/transport/httpapi"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		attrs := append(observability.ErrorAttrs(err), slog.String("command", commandName(os.Args[1:])))
		logger.LogAttrs(context.Background(), slog.LevelError, "application stopped", attrs...)
		os.Exit(1)
	}
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

type usageError struct{}

func (usageError) Error() string {
	return "usage: promo [serve|migrate|setup campaign.json|healthcheck]"
}

func (e usageError) LogAttrs() []slog.Attr {
	return []slog.Attr{slog.String("error_code", "invalid_arguments"), slog.String("reason", e.Error())}
}

func run(args []string) error {
	command := "serve"
	var campaign domain.Setup
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "healthcheck":
		if len(args) != 1 {
			return usageError{}
		}
		address, err := config.HTTPAddress()
		if err != nil {
			return err
		}
		return healthcheck.Check(context.Background(), address)
	case "serve", "migrate":
		if len(args) > 1 {
			return usageError{}
		}
	case "setup":
		if len(args) != 2 {
			return usageError{}
		}
		var err error
		campaign, err = config.LoadCampaign(args[1])
		if err != nil {
			return err
		}
	default:
		return usageError{}
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	if command == "serve" {
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
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	repo, err := postgres.New(connectCtx, c.DatabaseURL)
	cancel()
	if err != nil {
		return err
	}
	defer repo.Close()
	startupCtx, startupCancel := context.WithTimeout(ctx, time.Minute)
	defer startupCancel()
	if command == "migrate" {
		if err := repo.Migrate(startupCtx); err != nil {
			return err
		}
		logger.InfoContext(ctx, "database migrations completed")
		return nil
	}
	if command == "setup" {
		if err := repo.Setup(startupCtx, campaign); err != nil {
			return err
		}
		logger.InfoContext(ctx, "campaign imported", "campaign_ref", observability.Ref(campaign.Campaign.ID))
		return nil
	}
	if c.Mode == "production" {
		if _, err := repo.Campaign(startupCtx, c.CampaignID); err != nil {
			return fmt.Errorf("load configured campaign (run setup first): %w", err)
		}
	}
	platform, err := newBotPlatform(c)
	if err != nil {
		return err
	}
	startupCancel()
	service := app.New(repo, platform.messenger, app.RealClock{}, domain.CryptoRandom{}, app.WithLogger(logger))
	handler := httpapi.New(service, repo, platform.demo, platform.callbacks, httpapi.Config{
		Mode:          c.Mode,
		AdminToken:    c.AdminToken,
		DemoToken:     c.DemoToken,
		WebhookSecret: platform.webhookSecret,
		CampaignID:    c.CampaignID,
		BannerURL:     c.BannerURL,
	})
	server := &http.Server{
		Addr:              c.Address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	listener, err := net.Listen("tcp", c.Address)
	if err != nil {
		return err
	}
	defer listener.Close()
	workerDone := make(chan struct{})
	go func() {
		service.Work(ctx, logger)
		close(workerDone)
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	logger.Info("server started", "address", listener.Addr().String(), "mode", c.Mode)
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			<-workerDone
			return err
		}
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	err = server.Shutdown(shutdownCtx)
	<-workerDone
	logger.Info("shutdown complete")
	return err
}

type botPlatform struct {
	messenger     app.Messenger
	callbacks     httpapi.CallbackResponder
	demo          *demo.Messenger
	webhookSecret string
}

func newBotPlatform(c config.Config) (botPlatform, error) {
	platform := botPlatform{webhookSecret: c.WebhookSecret}
	if c.Mode == "demo" {
		platform.demo = demo.New()
		platform.messenger = platform.demo
		platform.callbacks = platform.demo
	} else {
		maxClient, err := maxapi.New(maxapi.Config{Token: c.MAXToken, BaseURL: c.MAXURL, BannerURL: c.BannerURL, CAFile: c.CAFile})
		if err != nil {
			return platform, err
		}
		platform.messenger = maxClient
		platform.callbacks = maxClient
	}
	return platform, nil
}
