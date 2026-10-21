package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMAXEventsMapToCommands(t *testing.T) {
	for _, test := range []struct {
		name, body, action string
		user               int64
		ignored, invalid   bool
	}{
		{"start", `{"update_type":"bot_started","timestamp":10,"chat_id":1,"user":{"user_id":42}}`, "start", 42, false, false},
		{"missing start user", `{"update_type":"bot_started","timestamp":10,"chat_id":1}`, "", 0, false, true},
		{"command", `{"update_type":"message_created","timestamp":1,"message":{"sender":{"user_id":42},"recipient":{"chat_type":"dialog"},"body":{"mid":"1","text":"/promo"}}}`, "promo", 42, false, false},
		{"name", `{"update_type":"message_created","timestamp":1,"message":{"sender":{"user_id":42},"recipient":{"chat_type":"dialog"},"body":{"mid":"1","text":"  Иван Иванов  "}}}`, "claim_name", 42, false, false},
		{"group", `{"update_type":"message_created","timestamp":1,"message":{"sender":{"user_id":42},"recipient":{"chat_type":"chat"},"body":{"mid":"1","text":"/promo"}}}`, "", 0, true, false},
		{"bot", `{"update_type":"message_created","timestamp":1,"message":{"sender":{"user_id":42,"is_bot":true},"recipient":{"chat_type":"dialog"},"body":{"mid":"1","text":"/promo"}}}`, "", 0, true, false},
		{"missing message id", `{"update_type":"message_created","timestamp":1,"message":{"sender":{"user_id":42},"recipient":{"chat_type":"dialog"},"body":{"text":"/promo"}}}`, "", 0, false, true},
		{"callback", `{"update_type":"message_callback","timestamp":1,"message":{"recipient":{"chat_type":"dialog"}},"callback":{"callback_id":"cb1","user":{"user_id":42},"payload":"participate"}}`, "participate", 42, false, false},
		{"deleted callback message", `{"update_type":"message_callback","timestamp":1,"message":null,"callback":{"callback_id":"cb1","user":{"user_id":42},"payload":"participate"}}`, "", 0, true, false},
		{"absent callback message", `{"update_type":"message_callback","timestamp":1,"callback":{"callback_id":"cb1","user":{"user_id":42},"payload":"participate"}}`, "", 0, true, false},
		{"null created message", `{"update_type":"message_created","timestamp":1,"message":null}`, "", 0, true, false},
		{"absent created message", `{"update_type":"message_created","timestamp":1}`, "", 0, true, false},
		{"claim", `{"update_type":"message_callback","timestamp":1,"message":{"recipient":{"chat_type":"dialog"}},"callback":{"callback_id":"cb1","user":{"user_id":42},"payload":"claim:offer1"}}`, "claim", 42, false, false},
		{"begin claim", `{"update_type":"message_callback","timestamp":1,"message":{"recipient":{"chat_type":"dialog"}},"callback":{"callback_id":"cb1","user":{"user_id":42},"payload":"claim_begin:offer1"}}`, "claim_begin", 42, false, false},
		{"invalid claim", `{"update_type":"message_callback","timestamp":1,"message":{"recipient":{"chat_type":"dialog"}},"callback":{"callback_id":"cb1","user":{"user_id":42},"payload":"claim:"}}`, "", 0, false, true},
		{"unexpected parameter", `{"update_type":"message_callback","timestamp":1,"message":{"recipient":{"chat_type":"dialog"}},"callback":{"callback_id":"cb1","user":{"user_id":42},"payload":"promo:injection"}}`, "", 0, false, true},
		{"unknown callback", `{"update_type":"message_callback","timestamp":1,"message":{"recipient":{"chat_type":"dialog"}},"callback":{"callback_id":"cb1","user":{"user_id":42},"payload":"danger"}}`, "", 0, false, true},
		{"unhandled update", `{"update_type":"chat_title_changed","timestamp":1}`, "", 0, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var update maxUpdate
			if err := json.Unmarshal([]byte(test.body), &update); err != nil {
				t.Fatal(err)
			}
			command, ignored, err := update.command("campaign")
			if ignored != test.ignored || (err != nil) != test.invalid {
				t.Fatalf("ignored=%v err=%v", ignored, err)
			}
			if err == nil && !ignored {
				if command.Action != test.action || command.UserID != test.user || command.ID == "" {
					t.Fatalf("unexpected command: %+v", command)
				}
				if command.Action == "claim" && !command.Consent {
					t.Fatal("consent button not recognized")
				}
				if command.Action == "claim_name" && command.Name != "Иван Иванов" {
					t.Fatal("name not trimmed")
				}
			}
		})
	}
}
func TestEventDeduplicationUsesStablePlatformIDAndCampaign(t *testing.T) {
	a := eventID("campaign", "callback:1")
	if a != eventID("campaign", "callback:1") || a == eventID("other", "callback:1") || a == eventID("campaign", "callback:2") {
		t.Fatal("invalid event identity")
	}
}

func TestMAXMessageIdentityIncludesMessageIDAndUser(t *testing.T) {
	var message maxUpdate
	_ = json.Unmarshal([]byte(`{"update_type":"message_created","timestamp":1,"message":{"sender":{"user_id":42},"recipient":{"chat_type":"dialog"},"body":{"mid":"message-1","text":"/promo"}}}`), &message)
	first, _, err := message.command("campaign")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := message.command("campaign")
	if err != nil || first.ID != second.ID {
		t.Fatal("same MAX message did not deduplicate")
	}
	message.Message.Sender.UserID = 43
	other, _, _ := message.command("campaign")
	if other.ID == first.ID {
		t.Fatal("message identities collided between users")
	}
}

func TestMAXCallbackIdentityIncludesTheCompleteEvent(t *testing.T) {
	var original maxUpdate
	if err := json.Unmarshal([]byte(callbackBody), &original); err != nil {
		t.Fatal(err)
	}
	identity := func(update maxUpdate, campaign string) string {
		t.Helper()
		command, ignored, err := update.command(campaign)
		if err != nil || ignored || command.ID == "" {
			t.Fatalf("valid callback was not accepted: ignored=%v error=%v command=%+v", ignored, err, command)
		}
		return command.ID
	}
	first := identity(original, "campaign")
	if repeated := identity(original, "campaign"); repeated != first {
		t.Fatal("identical callback did not deduplicate")
	}
	seen := map[string]bool{first: true}
	for _, test := range []struct {
		name   string
		change func(*maxUpdate)
	}{
		{"timestamp", func(u *maxUpdate) { u.Timestamp++ }},
		{"user", func(u *maxUpdate) { u.Callback.User.UserID++ }},
		{"callback ID", func(u *maxUpdate) { u.Callback.CallbackID += ":\"\\\n" }},
		{"payload", func(u *maxUpdate) { u.Callback.Payload = "help" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := original
			test.change(&changed)
			id := identity(changed, "campaign")
			if seen[id] {
				t.Fatal("different callback events shared an inbox identity")
			}
			seen[id] = true
		})
	}
	if identity(original, "another-campaign") == first {
		t.Fatal("callback identities collided between campaigns")
	}
	left, right := original, original
	left.Callback.CallbackID, left.Callback.Payload = "button:claim", "claim:offer"
	right.Callback.CallbackID, right.Callback.Payload = "button", "claim:claim:offer"
	if identity(left, "campaign") == identity(right, "campaign") {
		t.Fatal("callback delimiters changed the event field boundaries")
	}
}

func FuzzMAXUpdate(f *testing.F) {
	f.Add([]byte(`{"update_type":"bot_started","timestamp":1,"chat_id":1,"user":{"user_id":42}}`))
	f.Add([]byte(`{"update_type":"message_callback","timestamp":1,"message":{"recipient":{"chat_type":"dialog"}},"callback":{"callback_id":"id","user":{"user_id":42},"payload":"claim:offer"}}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		var update maxUpdate
		if err := json.Unmarshal(body, &update); err != nil {
			return
		}
		command, ignored, err := update.command("campaign")
		if err != nil || ignored {
			return
		}
		if command.ID == "" || command.CampaignID != "campaign" || command.UserID <= 0 || command.OccurredAt.IsZero() {
			t.Fatalf("invalid accepted command: %+v", command)
		}
		if command.Action == "claim" && (!command.Consent || command.OfferID == "") {
			t.Fatal("claim accepted without consent or offer")
		}
	})
}

func TestMAXSourceTimeIsMandatoryAndPreserved(t *testing.T) {
	for _, timestamp := range []int64{0, -1, 253402300800000} {
		var update maxUpdate
		body := strings.Replace(callbackBody, `"timestamp":1`, fmt.Sprintf(`"timestamp":%d`, timestamp), 1)
		if err := json.Unmarshal([]byte(body), &update); err != nil {
			t.Fatal(err)
		}
		if _, _, err := update.command("campaign"); err == nil {
			t.Fatalf("invalid MAX source timestamp accepted: %d", timestamp)
		}
	}
	var update maxUpdate
	if err := json.Unmarshal([]byte(strings.Replace(callbackBody, `,"timestamp":1`, "", 1)), &update); err != nil {
		t.Fatal(err)
	}
	if _, _, err := update.command("campaign"); err == nil {
		t.Fatal("missing MAX source timestamp used an internal fallback")
	}
	source := time.Date(2026, 10, 21, 9, 12, 34, 567000000, time.UTC)
	body := strings.Replace(callbackBody, `"timestamp":1`, fmt.Sprintf(`"timestamp":%d`, source.UnixMilli()), 1)
	if err := json.Unmarshal([]byte(body), &update); err != nil {
		t.Fatal(err)
	}
	command, _, err := update.command("campaign")
	if err != nil {
		t.Fatal(err)
	}
	if !command.OccurredAt.Equal(source) {
		t.Fatalf("timestamp was not bound exactly: %s", command.OccurredAt)
	}
}
