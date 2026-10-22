package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"promo/internal/app"
	"promo/internal/domain"
	"promo/internal/transport/httpapi"
)

func (r runtime) serve(ctx context.Context, stop context.CancelFunc, platform botPlatform) error {
	service := app.New(
		r.repo,
		platform.messenger,
		app.RealClock{},
		domain.CryptoRandom{},
		app.WithLogger(r.logger),
	)
	handler := httpapi.New(service, r.repo, platform.demo, platform.callbacks, httpapi.Config{
		Mode:          r.config.Mode,
		AdminToken:    r.config.AdminToken,
		DemoToken:     r.config.DemoToken,
		WebhookSecret: platform.webhookSecret,
		CampaignID:    r.config.CampaignID,
		BannerURL:     r.config.BannerURL,
	})

	return r.listenAndServe(ctx, stop, service, newHTTPServer(r.config.Address, handler))
}

func newHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}

func (r runtime) listenAndServe(ctx context.Context, stop context.CancelFunc, service *app.Service, server *http.Server) error {
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	defer listener.Close()

	workerDone := make(chan struct{})
	go func() {
		service.Work(ctx, r.logger)
		close(workerDone)
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	r.logger.Info("server started", "address", listener.Addr().String(), "mode", r.config.Mode)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			<-workerDone
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = server.Shutdown(shutdownCtx)
	<-workerDone
	r.logger.Info("shutdown complete")
	return err
}
