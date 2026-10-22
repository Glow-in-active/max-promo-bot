package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"promo/internal/domain"
)

func New(output io.Writer, value string) (*slog.Logger, error) {
	levels := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}
	level, ok := levels[strings.ToLower(strings.TrimSpace(value))]
	if !ok {
		return nil, errors.New("LOG_LEVEL must be debug, info, warn, or error")
	}
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level})), nil
}

func ErrorAttrs(err error) []slog.Attr {
	if err == nil {
		return nil
	}
	attrs := []slog.Attr{slog.String("error_type", fmt.Sprintf("%T", err))}
	var diagnostic interface{ LogAttrs() []slog.Attr }
	if errors.As(err, &diagnostic) {
		attrs = append(attrs, diagnostic.LogAttrs()...)
	}
	if fault, ok := domain.FaultOf(err); ok && asciiCode(string(fault.Code)) {
		attrs = append(attrs, slog.String("error_code", string(fault.Code)))
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) && asciiCode(state.SQLState()) {
		attrs = append(attrs, slog.String("sqlstate", state.SQLState()))
	}
	var upstream interface{ HTTPStatus() int }
	if errors.As(err, &upstream) && upstream.HTTPStatus() >= 100 && upstream.HTTPStatus() <= 599 {
		attrs = append(attrs, slog.Int("upstream_status", upstream.HTTPStatus()))
	}
	var file *os.PathError
	if errors.As(err, &file) {
		switch {
		case errors.Is(file, os.ErrNotExist):
			attrs = append(attrs, slog.String("error_code", "file_not_found"))
		case errors.Is(file, os.ErrPermission):
			attrs = append(attrs, slog.String("error_code", "permission_denied"))
		default:
			attrs = append(attrs, slog.String("error_code", "file_error"))
		}
		switch file.Op {
		case "open", "read", "stat", "close":
			attrs = append(attrs, slog.String("file_operation", file.Op))
		}
	}
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		attrs = append(attrs, slog.String("error_code", "invalid_json"), slog.Int64("json_offset", syntax.Offset))
	}
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		attrs = append(attrs, slog.String("error_code", "invalid_json_type"), slog.Int64("json_offset", mismatch.Offset))
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		attrs = append(attrs, slog.String("error_code", "deadline_exceeded"))
	case errors.Is(err, context.Canceled):
		attrs = append(attrs, slog.String("error_code", "canceled"))
	}
	return attrs
}

func Ref(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func asciiCode(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
