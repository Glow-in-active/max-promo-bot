package maxapi

import (
	"unicode/utf8"

	"promo/internal/domain"
)

func validateMessage(message domain.Message) error {
	if utf8.RuneCountInString(message.Text) > domain.MessageTextLimit {
		return messageValidationError("text exceeds 4000 characters")
	}
	for _, button := range message.Buttons {
		length := utf8.RuneCountInString(button.Text)
		if length < 1 || length > domain.ButtonTextLimit {
			return messageValidationError("button text must contain 1 to 128 characters")
		}
		if button.URL != "" {
			if utf8.RuneCountInString(button.URL) > domain.ButtonURLLimit {
				return messageValidationError("button URL exceeds 2048 characters")
			}
		} else if utf8.RuneCountInString(button.Payload) > domain.ButtonPayloadLimit {
			return messageValidationError("button payload exceeds 1024 characters")
		}
	}
	return nil
}
