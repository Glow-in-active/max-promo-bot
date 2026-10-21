package config

import "log/slog"

type validationError string

func (e validationError) Error() string { return string(e) }

func (e validationError) LogAttrs() []slog.Attr {
	return []slog.Attr{slog.String("error_code", "invalid_configuration"), slog.String("reason", string(e))}
}
