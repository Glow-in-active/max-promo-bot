package domain

import (
	"fmt"
	"unicode/utf8"
)

func validateSetupMessages(s Setup) error {
	if err := validateSetupText("Campaign rules", s.Campaign.Rules); err != nil {
		return err
	}

	days := make(map[string]Day, len(s.Days))
	activities := make([]Activity, 0, len(s.Days))
	for _, day := range s.Days {
		if err := validateSetupText("Participation message", ParticipationText(day)); err != nil {
			return err
		}
		days[day.ID] = day
		activities = append(activities, Activity{DayNumber: day.Number, Partner: day.Partner})
	}
	for _, promo := range s.Promos {
		if err := validateSetupText("Promo message", PromoText(promo, days[promo.DayID])); err != nil {
			return err
		}
	}
	for _, draw := range s.Draws {
		if err := validateSetupText("Prize notification", PrizeText(draw)); err != nil {
			return err
		}
	}
	for _, prefix := range []string{"", RegistrationConfirmed, RegistrationExists} {
		if err := validateSetupText("Chances message", ChancesText(s.Campaign, activities, 0, prefix)); err != nil {
			return err
		}
	}
	return nil
}

func validateSetupText(name, text string) error {
	if utf8.RuneCountInString(text) > MessageTextLimit {
		return Fail(Invalid, fmt.Sprintf("%s exceeds the MAX limit of %d characters.", name, MessageTextLimit))
	}
	return nil
}
