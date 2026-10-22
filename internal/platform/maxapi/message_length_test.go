package maxapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"promo/internal/domain"
)

func TestSendMessageTextBoundary(t *testing.T) {
	for _, character := range []string{"a", "Ж", "🎁"} {
		t.Run(character, func(t *testing.T) {
			calls := 0
			text := strings.Repeat(character, 4000)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct {
					Text string `json:"text"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Text != text {
					t.Error("message text changed in transit")
				}
				_, _ = w.Write([]byte(`{"message":{"body":{"mid":"sent"}}}`))
			})
			if err := client.Send(context.Background(), domain.Message{UserID: 42, Text: text}); err != nil {
				t.Fatalf("4000-character message rejected: %v", err)
			}
			err := client.Send(context.Background(), domain.Message{UserID: 42, Text: text + character})
			if err == nil {
				t.Fatal("4001-character message accepted")
			}
			failure, ok := err.(interface {
				Temporary() bool
				Uncertain() bool
			})
			if !ok || failure.Temporary() || failure.Uncertain() {
				t.Fatalf("local validation must be a permanent, certain failure: %v", err)
			}
			if strings.Contains(err.Error(), strings.Repeat(character, 20)) || calls != 1 {
				t.Fatalf("oversized message leaked or reached HTTP: calls=%d err=%v", calls, err)
			}
		})
	}
}
