package main

import (
	"promo/internal/app"
	"promo/internal/config"
	"promo/internal/platform/demo"
	"promo/internal/platform/maxapi"
	"promo/internal/transport/httpapi"
)

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
		return platform, nil
	}

	client, err := maxapi.New(maxapi.Config{
		Token:     c.MAXToken,
		BaseURL:   c.MAXURL,
		BannerURL: c.BannerURL,
		CAFile:    c.CAFile,
	})
	if err != nil {
		return platform, err
	}
	platform.messenger = client
	platform.callbacks = client
	return platform, nil
}
