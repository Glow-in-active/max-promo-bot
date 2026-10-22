package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"promo/internal/domain"
	"promo/internal/transport/httpapi/generated"
)

type maxUpdate generated.MAXUpdate

func (u maxUpdate) command(campaign string) (domain.Command, bool, error) {
	command := domain.Command{CampaignID: campaign}
	if u.UpdateType == "" || u.Timestamp <= 0 || u.Timestamp > 253402300799999 {
		return invalidMAXUpdate(command)
	}
	command.OccurredAt = time.UnixMilli(u.Timestamp).UTC()

	switch u.UpdateType {
	case "bot_started":
		return u.startCommand(command)
	case "message_created":
		return u.messageCommand(command)
	case "message_callback":
		return u.callbackCommand(command)
	default:
		return command, true, nil
	}
}

func (u maxUpdate) startCommand(command domain.Command) (domain.Command, bool, error) {
	if u.User.IsBot {
		return command, true, nil
	}
	if u.User.UserID <= 0 || u.ChatID == 0 {
		return invalidMAXUpdate(command)
	}

	command.ID = eventID(command.CampaignID, fmt.Sprintf("start:%d:%d:%d", u.User.UserID, u.ChatID, u.Timestamp))
	command.UserID = u.User.UserID
	command.Action = "start"
	return command, false, nil
}

func (u maxUpdate) messageCommand(command domain.Command) (domain.Command, bool, error) {
	if u.Message == nil || u.Message.Sender.IsBot || u.Message.Recipient.ChatType != "dialog" {
		return command, true, nil
	}
	if u.Message.Sender.UserID <= 0 {
		return invalidMAXUpdate(command)
	}

	body := u.Message.Body
	if body == nil {
		return command, true, nil
	}
	if body.Mid == "" || len(body.Mid) > 256 {
		return invalidMAXUpdate(command)
	}
	if body.Text == nil {
		return command, true, nil
	}

	text := strings.TrimSpace(*body.Text)
	if len([]rune(text)) > 4000 {
		return invalidMAXUpdate(command)
	}
	if text == "" {
		return command, true, nil
	}

	command.ID = eventID(command.CampaignID, fmt.Sprintf("message:%d:%s", u.Message.Sender.UserID, body.Mid))
	command.UserID = u.Message.Sender.UserID
	return textCommand(command, text), false, nil
}

func textCommand(command domain.Command, text string) domain.Command {
	switch strings.ToLower(text) {
	case "/start", "start":
		command.Action = "start"
	case "/promo":
		command.Action = "promo"
	case "/participate":
		command.Action = "participate"
	case "/check":
		command.Action = "check"
	case "/chances":
		command.Action = "chances"
	case "/help":
		command.Action = "help"
	case "/rules":
		command.Action = "rules"
	default:
		command.Action = "claim_name"
		command.Name = text
	}
	return command
}

func (u maxUpdate) callbackCommand(command domain.Command) (domain.Command, bool, error) {
	if u.Message == nil || u.Callback.User.IsBot || u.Message.Recipient.ChatType != "dialog" {
		return command, true, nil
	}
	if u.Callback.User.UserID <= 0 || u.Callback.CallbackID == "" ||
		len(u.Callback.CallbackID) > 256 || len(u.Callback.Payload) > 512 {
		return invalidMAXUpdate(command)
	}

	command.ID = eventID(command.CampaignID,
		fmt.Sprintf("callback:%d:%d:%q:%q", u.Timestamp, u.Callback.User.UserID, u.Callback.CallbackID, u.Callback.Payload))
	command.UserID = u.Callback.User.UserID
	return callbackPayloadCommand(command, u.Callback.Payload)
}

func callbackPayloadCommand(command domain.Command, payload string) (domain.Command, bool, error) {
	action, offerID, hasOffer := strings.Cut(payload, ":")
	switch action {
	case "start", "promo", "participate", "check", "chances", "help", "rules":
		if hasOffer {
			return invalidMAXUpdate(command)
		}
		command.Action = action
	case "claim_begin", "claim":
		if !hasOffer || offerID == "" || len(offerID) > 128 {
			return invalidMAXUpdate(command)
		}
		command.Action = action
		command.OfferID = offerID
		command.Consent = action == "claim"
	default:
		return invalidMAXUpdate(command)
	}
	return command, false, nil
}

func invalidMAXUpdate(command domain.Command) (domain.Command, bool, error) {
	return command, false, domain.Fail(domain.Invalid, "Invalid MAX update.")
}

func eventID(campaign, key string) string {
	sum := sha256.Sum256([]byte(campaign + ":" + key))
	return hex.EncodeToString(sum[:])
}
