package httpapi

import (
	"context"
	"encoding/csv"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"promo/internal/domain"
	"promo/internal/platform/demo"
)

type exportRepositoryStub struct {
	*repositoryStub
	snapshot domain.ExportSnapshot
}

func (s *exportRepositoryStub) Export(context.Context, string) (domain.ExportSnapshot, error) {
	return s.snapshot, nil
}

func TestExportKeepsConsentPrivateAndMapsOfferColumns(t *testing.T) {
	for _, status := range []string{"claimed", "pending", "notified", "expired"} {
		t.Run(status, func(t *testing.T) {
			draw, offer := exportFixture()
			offer.Status = status
			repo := &exportRepositoryStub{
				repositoryStub: &repositoryStub{},
				snapshot: domain.ExportSnapshot{
					Draws:  []domain.Draw{draw},
					Offers: []domain.Offer{offer},
				},
			}
			handler := New(&serviceStub{}, repo, demo.New(), demo.New(), Config{AdminToken: "admin"})
			response := request(handler, http.MethodGet, "/api/admin/campaigns/c/export.csv", "", "admin")
			if response.Code != http.StatusOK {
				t.Fatalf("CSV response: %d %s", response.Code, response.Body.String())
			}
			records, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			assertExportRows(t, records, status)
		})
	}
}

func exportFixture() (domain.Draw, domain.Offer) {
	drawn := time.Date(2026, 10, 21, 12, 0, 0, 0, time.FixedZone("local", 2*60*60))
	approved := drawn.Add(time.Minute)
	sent := approved.Add(time.Minute)
	deadline := sent.Add(time.Hour)
	claimed := sent.Add(5 * time.Minute)
	consent := claimed.Add(time.Second)
	draw := domain.Draw{
		ID:         "draw",
		Kind:       "daily",
		State:      domain.DrawApproved,
		Prize:      "=formula",
		DrawnAt:    &drawn,
		ApprovedAt: &approved,
		Candidates: []domain.Candidate{{UserID: 42, Weight: 3}},
		Selection:  domain.Selection{Winners: []int64{42}, Reserve: []int64{43}},
	}
	offer := domain.Offer{
		ID:            "offer",
		SlotID:        "slot",
		DrawID:        draw.ID,
		UserID:        42,
		Status:        "claimed",
		DeliveryState: "delivered",
		SentAt:        &sent,
		Deadline:      &deadline,
		ClaimedAt:     &claimed,
		ConsentAt:     &consent,
		Name:          " \t=SUM(1,2)",
	}
	return draw, offer
}

func assertExportRows(t *testing.T, records [][]string, status string) {
	t.Helper()
	if len(records) != 5 {
		t.Fatalf("expected header, participant, winner, reserve and offer; got %d records", len(records))
	}
	want := map[string]string{
		"record_type":        "offer",
		"draw_id":            "draw",
		"kind":               "daily",
		"draw_state":         "approved",
		"selection_list":     "",
		"selection_position": "",
		"user_id":            "42",
		"weight":             "3",
		"prize":              "'=formula",
		"slot_id":            "slot",
		"offer_id":           "offer",
		"offer_status":       status,
		"delivery_state":     "delivered",
		"drawn_at":           "2026-10-21T10:00:00Z",
		"approved_at":        "2026-10-21T10:01:00Z",
		"sent_at":            "2026-10-21T10:02:00Z",
		"deadline":           "2026-10-21T11:02:00Z",
		"claimed_at":         "2026-10-21T10:07:00Z",
		"consent_at":         "",
		"name":               "",
	}
	if status == "claimed" {
		want["consent_at"] = "2026-10-21T10:07:01Z"
		want["name"] = "' \t=SUM(1,2)"
	}
	offers := 0
	for _, record := range records[1:] {
		row := csvRowByHeader(t, records[0], record)
		if row["record_type"] == "offer" {
			offers++
			if !reflect.DeepEqual(row, want) {
				t.Fatalf("offer columns: got %+v, want %+v", row, want)
			}
			continue
		}
		for _, column := range []string{
			"slot_id", "offer_id", "offer_status", "delivery_state",
			"sent_at", "deadline", "claimed_at", "consent_at", "name",
		} {
			if row[column] != "" {
				t.Fatalf("%s row contains offer data in %s: %q", row["record_type"], column, row[column])
			}
		}
	}
	if offers != 1 {
		t.Fatalf("expected one offer record, got %d", offers)
	}
}

func csvRowByHeader(t *testing.T, header, record []string) map[string]string {
	t.Helper()
	if len(record) != len(header) {
		t.Fatalf("CSV width: record has %d columns, header has %d", len(record), len(header))
	}
	row := make(map[string]string, len(header))
	for i, name := range header {
		if _, exists := row[name]; exists {
			t.Fatalf("duplicate CSV column %q", name)
		}
		row[name] = record[i]
	}
	return row
}
