package domain

import (
	"net/url"
	"strings"
	"time"
)

type Setup struct {
	Campaign        Campaign `json:"campaign"`
	Days            []Day    `json:"days"`
	Draws           []Draw   `json:"draws"`
	Promos          []Promo  `json:"promos"`
	ClaimTTLSeconds int64    `json:"claim_ttl_seconds"`
}

func (s Setup) Prepare() (Setup, error) {
	if s.ClaimTTLSeconds < 1 || s.ClaimTTLSeconds > 365*24*60*60 {
		return Setup{}, Fail(Invalid, "Prize claim period must be between one second and one year.")
	}
	s.Draws = append([]Draw(nil), s.Draws...)
	for i := range s.Draws {
		s.Draws[i].ClaimTTL = time.Duration(s.ClaimTTLSeconds) * time.Second
	}
	return s, ValidateSetup(s)
}

func ValidateSetup(s Setup) error {
	c := s.Campaign
	if c.ID == "" || strings.TrimSpace(c.Name) == "" || c.Status != "active" || !c.StartsAt.Before(c.EndsAt) || c.FinalDrawID == "" || strings.TrimSpace(c.Rules) == "" || strings.TrimSpace(c.MainPrize) == "" {
		return Fail(Invalid, "Campaign configuration is incomplete or invalid.")
	}
	if c.Timezone == "" || c.Timezone == "Local" {
		return Fail(Invalid, "Campaign timezone must be an explicit IANA timezone name.")
	}
	location, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return Fail(Invalid, "Invalid campaign timezone.")
	}
	if len(s.Days) != 10 {
		return Fail(Invalid, "Campaign must contain exactly ten days.")
	}
	dayIDs := map[string]bool{}
	dayNumbers := map[int]bool{}
	dayDates := map[string]bool{}
	drawIDs := map[string]Draw{}
	for _, d := range s.Draws {
		if d.ID == "" || drawIDs[d.ID].ID != "" || d.CampaignID != c.ID || d.State != DrawOpen || d.WinnerCount < 1 || d.ReserveCount < 0 || d.ClaimTTL <= 0 || strings.TrimSpace(d.Prize) == "" || d.DrawnAt != nil || d.ApprovedAt != nil || len(d.Candidates) != 0 || len(d.Selection.Winners) != 0 || len(d.Selection.Reserve) != 0 {
			return Fail(Invalid, "Invalid draw configuration.")
		}
		drawIDs[d.ID] = d
	}
	if len(s.Draws) != 11 {
		return Fail(Invalid, "Ten daily draws and one final draw are required.")
	}
	final, ok := drawIDs[c.FinalDrawID]
	if !ok || final.Kind != "final" || final.DayID != "" || final.WinnerCount != 5 || final.ReserveCount < 1 || final.ClosesAt.Before(c.EndsAt) {
		return Fail(Invalid, "Invalid final draw configuration.")
	}
	for _, d := range s.Days {
		if d.ID == "" || dayIDs[d.ID] || dayNumbers[d.Number] || d.Number < 1 || d.Number > 10 || d.CampaignID != c.ID || !d.StartsAt.Before(d.EndsAt) || d.StartsAt.Before(c.StartsAt) || d.EndsAt.After(c.EndsAt) || strings.TrimSpace(d.Partner) == "" || d.ChannelID == 0 || strings.TrimSpace(d.Instruction) == "" {
			return Fail(Invalid, "Invalid campaign day configuration.")
		}
		localStart := d.StartsAt.In(location)
		nextDate := time.Date(localStart.Year(), localStart.Month(), localStart.Day()+1, 0, 0, 0, 0, location)
		if d.EndsAt.After(nextDate) {
			return Fail(Invalid, "Campaign day must stay within one calendar date in the campaign timezone.")
		}
		date := localStart.Format(time.DateOnly)
		if dayDates[date] {
			return Fail(Invalid, "Campaign calendar dates must be unique.")
		}
		dayDates[date] = true
		partnerURL, err := url.Parse(d.PartnerURL)
		if err != nil || partnerURL.Scheme != "https" || partnerURL.Hostname() == "" || partnerURL.User != nil {
			return Fail(Invalid, "Partner link must be an absolute HTTPS URL without credentials.")
		}
		for _, other := range s.Days {
			if d.ID != other.ID && d.StartsAt.Before(other.EndsAt) && other.StartsAt.Before(d.EndsAt) {
				return Fail(Invalid, "Campaign days overlap.")
			}
		}
		draw, ok := drawIDs[d.DrawID]
		if !ok || draw.Kind != "daily" || draw.DayID != d.ID || !draw.ClosesAt.Equal(d.EndsAt) {
			return Fail(Invalid, "Daily draw must close at the end of its campaign day.")
		}
		dayIDs[d.ID] = true
		dayNumbers[d.Number] = true
	}
	codes := map[string]bool{}
	ids := map[string]bool{}
	for _, p := range s.Promos {
		if p.ID == "" || strings.TrimSpace(p.Value) == "" || !dayIDs[p.DayID] || p.UserID != 0 || codes[p.Value] || ids[p.ID] {
			return Fail(Invalid, "Promo codes must be unique and unassigned.")
		}
		codes[p.Value] = true
		ids[p.ID] = true
	}
	return nil
}
