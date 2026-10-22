package maxapi

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"promo/internal/domain"
)

func TestSendButtonBoundaries(t *testing.T) {
	const partnerURL = "https://max.ru/"
	for _, character := range []string{"a", "Ж", "🎁"} {
		longURL := partnerURL + strings.Repeat(character, 2048-len(partnerURL))
		for _, test := range []struct {
			name   string
			button domain.Button
			valid  bool
		}{
			{name: "empty text", button: domain.Button{Payload: "check"}},
			{name: "empty callback payload", button: domain.Button{Text: "Check"}, valid: true},
			{
				name: "callback at limits", valid: true,
				button: domain.Button{Text: strings.Repeat(character, 128), Payload: strings.Repeat(character, 1024)},
			},
			{
				name:   "callback text overflow",
				button: domain.Button{Text: strings.Repeat(character, 129), Payload: "check"},
			},
			{
				name:   "callback payload overflow",
				button: domain.Button{Text: "Check", Payload: strings.Repeat(character, 1025)},
			},
			{
				name: "link at limits", valid: true,
				button: domain.Button{Text: strings.Repeat(character, 128), URL: longURL},
			},
			{
				name:   "link text overflow",
				button: domain.Button{Text: strings.Repeat(character, 129), URL: partnerURL},
			},
			{
				name:   "link URL overflow",
				button: domain.Button{Text: "Subscribe", URL: longURL + character},
			},
			{
				name: "link omits callback payload", valid: true,
				button: domain.Button{Text: "Subscribe", URL: partnerURL, Payload: strings.Repeat(character, 1025)},
			},
		} {
			t.Run(character+"/"+test.name, func(t *testing.T) {
				assertButtonDelivery(t, test.button, test.valid)
			})
		}
	}
}

func assertButtonDelivery(t *testing.T, source domain.Button, valid bool) {
	t.Helper()
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Attachments []struct {
				Payload struct {
					Buttons [][]button `json:"buttons"`
				} `json:"payload"`
			} `json:"attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if len(body.Attachments) != 1 || len(body.Attachments[0].Payload.Buttons) != 1 || len(body.Attachments[0].Payload.Buttons[0]) != 1 {
			t.Errorf("button missing from request: %+v", body)
			return
		}
		got := body.Attachments[0].Payload.Buttons[0][0]
		want := button{Type: "callback", Text: source.Text, Payload: &source.Payload}
		if source.URL != "" {
			want.Type, want.Payload, want.URL = "link", nil, source.URL
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("button changed in transit: got=%+v want=%+v", got, want)
		}
		_, _ = w.Write([]byte(`{"message":{"body":{"mid":"sent"}}}`))
	})
	err := client.Send(context.Background(), domain.Message{UserID: 42, Text: "Test", Buttons: []domain.Button{source}})
	if valid {
		if err != nil || calls.Load() != 1 {
			t.Fatalf("valid button rejected: calls=%d err=%v", calls.Load(), err)
		}
		return
	}
	failure, ok := err.(messageValidationError)
	if !ok || failure.Temporary() || failure.Uncertain() || calls.Load() != 0 {
		t.Fatalf("invalid button reached HTTP or was not a permanent local failure: calls=%d err=%v", calls.Load(), err)
	}
}
