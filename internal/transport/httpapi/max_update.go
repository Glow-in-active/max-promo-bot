package httpapi

import (
	"fmt"
	"strings"
	"time"

	"promo/internal/domain"
	"promo/internal/transport/httpapi/generated"
)

type maxUpdate generated.MAXUpdate

func (u maxUpdate) command(campaign string) (domain.Command, bool, error) {
	c := domain.Command{CampaignID: campaign}
	invalid := func() (domain.Command, bool, error) {
		return c, false, domain.Fail(domain.Invalid, "Invalid MAX update.")
	}
	if u.UpdateType == "" || u.Timestamp <= 0 || u.Timestamp > 253402300799999 {
		return invalid()
	}
	c.OccurredAt = time.UnixMilli(u.Timestamp).UTC()
	switch u.UpdateType {
	case "bot_started":
		if u.User.IsBot {
			return c, true, nil
		}
		if u.User.UserID <= 0 || u.ChatID == 0 {
			return invalid()
		}
		c.ID = eventID(campaign, fmt.Sprintf("start:%d:%d:%d", u.User.UserID, u.ChatID, u.Timestamp))
		c.UserID = u.User.UserID
		c.Action = "start"
	case "message_created":
		if u.Message == nil || u.Message.Sender.IsBot || u.Message.Recipient.ChatType != "dialog" {
			return c, true, nil
		}
		if u.Message.Sender.UserID <= 0 {
			return invalid()
		}
		body := u.Message.Body
		if body == nil {
			return c, true, nil
		}
		if body.Mid == "" || len(body.Mid) > 256 {
			return invalid()
		}
		if body.Text == nil {
			return c, true, nil
		}
		text := strings.TrimSpace(*body.Text)
		if len([]rune(text)) > 4000 {
			return invalid()
		}
		if text == "" {
			return c, true, nil
		}
		c.ID = eventID(campaign, fmt.Sprintf("message:%d:%s", u.Message.Sender.UserID, body.Mid))
		c.UserID = u.Message.Sender.UserID
		switch strings.ToLower(text) {
		case "/start", "start":
			c.Action = "start"
		case "/promo":
			c.Action = "promo"
		case "/participate":
			c.Action = "participate"
		case "/check":
			c.Action = "check"
		case "/chances":
			c.Action = "chances"
		case "/help":
			c.Action = "help"
		case "/rules":
			c.Action = "rules"
		default:
			c.Action = "claim_name"
			c.Name = text
		}
	case "message_callback":
		if u.Message == nil || u.Callback.User.IsBot || u.Message.Recipient.ChatType != "dialog" {
			return c, true, nil
		}
		if u.Callback.User.UserID <= 0 || u.Callback.CallbackID == "" || len(u.Callback.CallbackID) > 256 || len(u.Callback.Payload) > 512 {
			return invalid()
		}
		c.ID = eventID(campaign, fmt.Sprintf("callback:%d:%d:%q:%q", u.Timestamp, u.Callback.User.UserID, u.Callback.CallbackID, u.Callback.Payload))
		c.UserID = u.Callback.User.UserID
		action, offerID, hasOffer := strings.Cut(u.Callback.Payload, ":")
		switch action {
		case "start", "promo", "participate", "check", "chances", "help", "rules":
			if hasOffer {
				return invalid()
			}
			c.Action = action
		case "claim_begin", "claim":
			if !hasOffer || offerID == "" || len(offerID) > 128 {
				return invalid()
			}
			c.Action = action
			c.OfferID = offerID
			c.Consent = action == "claim"
		default:
			return invalid()
		}
	default:
		return c, true, nil
	}
	return c, false, nil
}
