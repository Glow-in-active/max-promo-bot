package httpapi

import (
	"encoding/csv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"promo/internal/domain"
	"promo/internal/transport/httpapi/generated"
)

type exportRecord struct {
	kind     string
	draw     domain.Draw
	userID   int64
	weight   int
	list     string
	position int
	offer    *domain.Offer
}

// Results are assembled before sending headers so a storage failure produces a
// normal error response rather than a partially successful CSV download.
func (s *server) ExportDrawResults(w http.ResponseWriter, r *http.Request, campaign generated.Campaign) {
	snapshot, err := s.repo.Export(r.Context(), campaign)
	if err != nil {
		handleError(w, err)
		return
	}
	records := exportRecords(snapshot)

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="draw-results.csv"`)
	writer := csv.NewWriter(w)
	_ = writer.Write([]string{
		"record_type", "draw_id", "kind", "draw_state", "selection_list",
		"selection_position", "user_id", "weight", "prize", "slot_id",
		"offer_id", "offer_status", "delivery_state", "drawn_at", "approved_at",
		"sent_at", "deadline", "claimed_at", "consent_at", "name",
	})
	for _, record := range records {
		if err := writer.Write(record.cells()); err != nil {
			return
		}
	}
	writer.Flush()
}

func exportRecords(snapshot domain.ExportSnapshot) []exportRecord {
	draws := snapshot.Draws
	sort.Slice(draws, func(i, j int) bool { return draws[i].ID < draws[j].ID })
	var records []exportRecord
	for _, draw := range draws {
		weights := make(map[int64]int, len(draw.Candidates))
		for _, candidate := range draw.Candidates {
			weights[candidate.UserID] = candidate.Weight
			records = append(records, exportRecord{
				kind:   "participant",
				draw:   draw,
				userID: candidate.UserID,
				weight: candidate.Weight,
			})
		}

		for _, list := range []struct {
			name  string
			users []int64
		}{
			{"winner", draw.Selection.Winners},
			{"reserve", draw.Selection.Reserve},
		} {
			for position, userID := range list.users {
				records = append(records, exportRecord{
					kind:     "selection",
					draw:     draw,
					userID:   userID,
					weight:   weights[userID],
					list:     list.name,
					position: position + 1,
				})
			}
		}

		for _, offer := range snapshot.Offers {
			if offer.DrawID != draw.ID {
				continue
			}
			records = append(records, exportRecord{
				kind:   "offer",
				draw:   draw,
				userID: offer.UserID,
				weight: weights[offer.UserID],
				offer:  &offer,
			})
		}
	}
	return records
}

func (r exportRecord) cells() []string {
	position := ""
	if r.position > 0 {
		position = strconv.Itoa(r.position)
	}
	offer := domain.Offer{}
	if r.offer != nil {
		offer = *r.offer
	}
	var consent, name string
	if offer.Status == "claimed" {
		consent = csvTime(offer.ConsentAt)
		name = offer.Name
	}

	values := []string{
		r.kind,
		r.draw.ID,
		r.draw.Kind,
		string(r.draw.State),
		r.list,
		position,
		strconv.FormatInt(r.userID, 10),
		strconv.Itoa(r.weight),
		r.draw.Prize,
		offer.SlotID,
		offer.ID,
		offer.Status,
		offer.DeliveryState,
		csvTime(r.draw.DrawnAt),
		csvTime(r.draw.ApprovedAt),
		csvTime(offer.SentAt),
		csvTime(offer.Deadline),
		csvTime(offer.ClaimedAt),
		consent,
		name,
	}
	for i, value := range values {
		values[i] = csvCell(value)
	}
	return values
}

func csvTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func csvCell(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}
