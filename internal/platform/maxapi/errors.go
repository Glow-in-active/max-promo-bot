package maxapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

type configurationError string

func (e configurationError) Error() string {
	return string(e)
}

func (e configurationError) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("error_code", "invalid_max_configuration"),
		slog.String("reason", string(e)),
	}
}

// Error carries safe metadata only. The response body can contain personal data
// and is deliberately excluded from errors and logs.
type Error struct {
	Status     int
	RetryAfter time.Duration
	Ambiguous  bool
	cause      error
}

func (e *Error) HTTPStatus() int {
	return e.Status
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return "MAX request failed before a delivery acknowledgement"
	}
	return fmt.Sprintf("MAX returned HTTP %d", e.Status)
}

func (e *Error) Temporary() bool {
	return e.Status == 0 || e.Status == http.StatusTooManyRequests || e.Status >= 500
}

func (e *Error) Uncertain() bool {
	return e.Ambiguous
}

func (e *Error) Unwrap() error {
	return e.cause
}

func (e *Error) RetryDelay() time.Duration {
	return e.RetryAfter
}

func requestError(ctx context.Context, err error, sending bool) *Error {
	cause := ctx.Err()
	if cause == nil {
		var timeout interface{ Timeout() bool }
		switch {
		case errors.Is(err, context.Canceled):
			cause = context.Canceled
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
			cause = context.DeadlineExceeded
		}
	}
	return &Error{Ambiguous: sending, cause: cause}
}
