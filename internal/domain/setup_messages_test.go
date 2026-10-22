package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSetupMessageBoundaries(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*Setup, string)
		text func(Setup) string
	}{
		{
			name: "rules",
			set:  func(s *Setup, text string) { s.Campaign.Rules = text },
			text: func(s Setup) string { return s.Campaign.Rules },
		},
		{
			name: "promo code",
			set:  func(s *Setup, text string) { s.Promos[0].Value = text },
			text: func(s Setup) string { return PromoText(s.Promos[0], s.Days[0]) },
		},
		{
			name: "promo instruction",
			set:  func(s *Setup, text string) { s.Days[0].Instruction = text },
			text: func(s Setup) string { return PromoText(s.Promos[0], s.Days[0]) },
		},
		{
			name: "daily prize",
			set:  func(s *Setup, text string) { s.Draws[0].Prize = text },
			text: func(s Setup) string { return PrizeText(s.Draws[0]) },
		},
		{
			name: "final prize",
			set:  func(s *Setup, text string) { s.Draws[len(s.Draws)-1].Prize = text },
			text: func(s Setup) string { return PrizeText(s.Draws[len(s.Draws)-1]) },
		},
		{
			name: "main prize in registration summary",
			set:  func(s *Setup, text string) { s.Campaign.MainPrize = text },
			text: setupRegistrationSummary,
		},
		{
			name: "partner in registration summary",
			set:  func(s *Setup, text string) { s.Days[0].Partner = text },
			text: setupRegistrationSummary,
		},
	} {
		for _, character := range []string{"x", "Я", "🎁"} {
			t.Run(test.name+"/"+character, func(t *testing.T) {
				s := validSetup(t)
				test.set(&s, "")
				budget := MessageTextLimit - utf8.RuneCountInString(test.text(s))
				test.set(&s, strings.Repeat(character, budget))
				if length := utf8.RuneCountInString(test.text(s)); length != 4000 {
					t.Fatalf("boundary message has %d characters", length)
				}
				if err := ValidateSetup(s); err != nil {
					t.Fatalf("4000-character message rejected: %v", err)
				}
				test.set(&s, strings.Repeat(character, budget+1))
				faultCode(t, ValidateSetup(s), Invalid)
			})
		}
	}
}

func TestSetupRejectsCombinedMessageOverflow(t *testing.T) {
	s := validSetup(t)
	s.Days[0].Partner = strings.Repeat("П", 1000)
	s.Days[0].Instruction = strings.Repeat("И", 1500)
	s.Promos[0].Value = strings.Repeat("К", 1500)
	if length := utf8.RuneCountInString(PromoText(s.Promos[0], s.Days[0])); length <= 4000 {
		t.Fatal("fixture must overflow only after composing the promo message")
	}
	faultCode(t, ValidateSetup(s), Invalid)
}

func TestSetupSummaryIncludesRegistrationPrefix(t *testing.T) {
	s := validSetup(t)
	s.Campaign.MainPrize = ""
	budget := MessageTextLimit - utf8.RuneCountInString(setupRegistrationSummary(s))
	s.Campaign.MainPrize = strings.Repeat("Б", budget)
	for _, prefix := range []string{"", RegistrationExists, RegistrationConfirmed} {
		text := ChancesText(s.Campaign, setupActivities(s), 0, prefix)
		if utf8.RuneCountInString(text) > 4000 {
			t.Fatalf("valid setup overflows summary with prefix %q", prefix)
		}
	}
	if err := ValidateSetup(s); err != nil {
		t.Fatal(err)
	}
	s.Campaign.MainPrize += "Б"
	if utf8.RuneCountInString(ChancesText(s.Campaign, setupActivities(s), 0, "")) > 4000 {
		t.Fatal("summary must fit without the registration prefix")
	}
	faultCode(t, ValidateSetup(s), Invalid)
}

func TestSetupSummaryCoversPartialActivity(t *testing.T) {
	s := validSetup(t)
	s.Days[0].Partner = strings.Repeat("🎁", 1200)
	s.Campaign.MainPrize = ""
	budget := MessageTextLimit - utf8.RuneCountInString(setupRegistrationSummary(s))
	s.Campaign.MainPrize = strings.Repeat("Б", budget)
	if err := ValidateSetup(s); err != nil {
		t.Fatal(err)
	}
	all := setupActivities(s)
	for mask := range 1 << len(all) {
		var active []Activity
		for day, activity := range all {
			if mask&(1<<day) != 0 {
				active = append(active, activity)
			}
		}
		for _, prefix := range []string{"", RegistrationExists, RegistrationConfirmed} {
			text := ChancesText(s.Campaign, active, len(all)-len(active), prefix)
			if length := utf8.RuneCountInString(text); length > 4000 {
				t.Fatalf("activity mask %010b produces %d characters with prefix %q", mask, length, prefix)
			}
		}
	}
}

func TestSetupPartnerURLBoundary(t *testing.T) {
	const prefix = "https://max.ru/"
	for _, character := range []string{"x", "Я", "🎁"} {
		t.Run(character, func(t *testing.T) {
			s := validSetup(t)
			s.Days[0].PartnerURL = prefix + strings.Repeat(character, 2048-len(prefix))
			if err := ValidateSetup(s); err != nil {
				t.Fatalf("2048-character partner URL rejected: %v", err)
			}
			s.Days[0].PartnerURL += character
			faultCode(t, ValidateSetup(s), Invalid)
		})
	}
}

func setupRegistrationSummary(s Setup) string {
	return ChancesText(s.Campaign, setupActivities(s), 0, RegistrationConfirmed)
}

func setupActivities(s Setup) []Activity {
	active := make([]Activity, 0, len(s.Days))
	for _, day := range s.Days {
		active = append(active, Activity{DayNumber: day.Number, Partner: day.Partner})
	}
	return active
}
