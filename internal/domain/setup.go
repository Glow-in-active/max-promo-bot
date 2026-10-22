package domain

import "time"

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
	location, err := validateCampaignSetup(s.Campaign)
	if err != nil {
		return err
	}
	if len(s.Days) != 10 {
		return Fail(Invalid, "Campaign must contain exactly ten days.")
	}

	draws, err := validateSetupDraws(s.Campaign, s.Draws)
	if err != nil {
		return err
	}
	dayIDs, err := validateSetupDays(s, draws, location)
	if err != nil {
		return err
	}

	if err := validateSetupPromos(s.Promos, dayIDs); err != nil {
		return err
	}
	return validateSetupMessages(s)
}
