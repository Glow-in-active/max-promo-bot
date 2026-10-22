package main

import (
	"context"
	"log/slog"
	"os"

	"promo/internal/observability"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		attrs := append(
			observability.ErrorAttrs(err),
			slog.String("command", commandName(os.Args[1:])),
		)
		logger.LogAttrs(context.Background(), slog.LevelError, "application stopped", attrs...)
		os.Exit(1)
	}
}
