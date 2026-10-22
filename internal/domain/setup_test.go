package domain

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func validSetup(t *testing.T) Setup {
	t.Helper()
	data, err := os.ReadFile("../../config/campaign.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var s Setup
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	s, err = s.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProductionSetupImport(t *testing.T) {
	s := validSetup(t)
	if s.Draws[0].ClaimTTL != 24*time.Hour {
		t.Fatal("TTL unit conversion failed")
	}
	if err := ValidateSetup(s); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidSetup(t *testing.T) {
	cases := map[string]func(*Setup){
		"campaign":       func(s *Setup) { s.Campaign.Name = "" },
		"timezone":       func(s *Setup) { s.Campaign.Timezone = "invalid-zone" },
		"timezone-empty": func(s *Setup) { s.Campaign.Timezone = "" },
		"timezone-local": func(s *Setup) { s.Campaign.Timezone = "Local" },
		"days":           func(s *Setup) { s.Days = s.Days[:9] },
		"draws":          func(s *Setup) { s.Draws = s.Draws[:10] },
		"draw-prize":     func(s *Setup) { s.Draws[0].Prize = "" },
		"draw-ttl":       func(s *Setup) { s.Draws[0].ClaimTTL = 0 },
		"draw-state":     func(s *Setup) { s.Draws[0].State = DrawApproved },
		"duplicate-draw": func(s *Setup) { s.Draws[1].ID = s.Draws[0].ID },
		"final-winners": func(s *Setup) {
			for i := range s.Draws {
				if s.Draws[i].Kind == "final" {
					s.Draws[i].WinnerCount = 10
				}
			}
		},
		"final-without-reserve": func(s *Setup) {
			for i := range s.Draws {
				if s.Draws[i].Kind == "final" {
					s.Draws[i].ReserveCount = 0
				}
			}
		},
		"day-partner":      func(s *Setup) { s.Days[0].Partner = "" },
		"partner-link":     func(s *Setup) { s.Days[0].PartnerURL = "not-a-url" },
		"partner-http":     func(s *Setup) { s.Days[0].PartnerURL = "http://max.ru/partner" },
		"partner-userinfo": func(s *Setup) { s.Days[0].PartnerURL = "https://user:password@max.ru/partner" },
		"blank-rules":      func(s *Setup) { s.Campaign.Rules = " \t\n" },
		"blank-prize":      func(s *Setup) { s.Draws[0].Prize = "  " },
		"blank-promo":      func(s *Setup) { s.Promos[0].Value = "  " },
		"overlap":          func(s *Setup) { s.Days[1].StartsAt = s.Days[0].StartsAt },
		"daily-cutoff":     func(s *Setup) { s.Draws[0].ClosesAt = s.Draws[0].ClosesAt.Add(time.Minute) },
		"promo-empty":      func(s *Setup) { s.Promos[0].Value = "" },
		"promo-duplicate":  func(s *Setup) { s.Promos = append(s.Promos, s.Promos[0]) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := validSetup(t)
			change(&s)
			faultCode(t, ValidateSetup(s), Invalid)
		})
	}
	for _, ttl := range []int64{0, -1, 366 * 24 * 60 * 60} {
		s := Setup{ClaimTTLSeconds: ttl}
		_, err := s.Prepare()
		faultCode(t, err, Invalid)
	}
}

func TestSetupAcceptsExplicitTimezones(t *testing.T) {
	for _, timezone := range []string{"UTC", "Europe/Moscow"} {
		t.Run(timezone, func(t *testing.T) {
			s := validSetup(t)
			location := setupLocation(t, timezone)
			setCalendarDays(&s, time.Date(2026, 11, 1, 0, 0, 0, 0, location))
			if err := ValidateSetup(s); err != nil {
				t.Fatalf("valid timezone rejected: %v", err)
			}
		})
	}
}

func TestSetupRequiresExplicitTimezone(t *testing.T) {
	for _, timezone := range []string{"", "Local"} {
		t.Run(timezone, func(t *testing.T) {
			s := validSetup(t)
			setCalendarDays(&s, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
			s.Campaign.Timezone = timezone
			faultCode(t, ValidateSetup(s), Invalid)
		})
	}
}

func TestSetupRejectsTwoPartnerPeriodsOnOneCalendarDate(t *testing.T) {
	for _, split := range []struct {
		name                             string
		firstEnd, secondStart, secondEnd time.Duration
	}{
		{name: "adjacent periods", firstEnd: 12 * time.Hour, secondStart: 12 * time.Hour, secondEnd: 24 * time.Hour},
		{name: "different UTC dates", firstEnd: 2 * time.Hour, secondStart: 4 * time.Hour, secondEnd: 6 * time.Hour},
	} {
		t.Run(split.name, func(t *testing.T) {
			s := validSetup(t)
			start := s.Days[0].StartsAt
			setSetupPeriod(&s, 0, start, start.Add(split.firstEnd))
			setSetupPeriod(&s, 1, start.Add(split.secondStart), start.Add(split.secondEnd))
			if !s.Days[0].EndsAt.Equal(s.Days[1].StartsAt) && !s.Days[0].EndsAt.Before(s.Days[1].StartsAt) {
				t.Fatal("partner periods overlap")
			}
			if split.name == "different UTC dates" && s.Days[0].StartsAt.UTC().Format(time.DateOnly) == s.Days[1].StartsAt.UTC().Format(time.DateOnly) {
				t.Fatal("periods must start on different UTC dates")
			}
			faultCode(t, ValidateSetup(s), Invalid)
		})
	}
}

func TestSetupCalendarDayBoundaries(t *testing.T) {
	location := setupLocation(t, "Europe/Moscow")
	for _, tc := range []struct {
		name       string
		start, end time.Duration
		valid      bool
	}{
		{name: "whole day", start: 0, end: 24 * time.Hour, valid: true},
		{name: "short period crossing UTC midnight", start: time.Hour, end: 7 * time.Hour, valid: true},
		{name: "last nanosecond", start: 23 * time.Hour, end: 24*time.Hour - time.Nanosecond, valid: true},
		{name: "one nanosecond after local midnight", start: 23 * time.Hour, end: 24*time.Hour + time.Nanosecond},
		{name: "crossing local midnight", start: 23 * time.Hour, end: 25 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := validSetup(t)
			start := time.Date(2026, 11, 1, 0, 0, 0, 0, location)
			setCalendarDays(&s, start)
			setSetupPeriod(&s, 0, start.Add(tc.start).UTC(), start.Add(tc.end).UTC())
			setSetupPeriod(&s, 1, start.AddDate(0, 0, 1).Add(2*time.Hour), s.Days[1].EndsAt)
			err := ValidateSetup(s)
			if tc.valid {
				if err != nil {
					t.Fatalf("valid local day rejected: %v", err)
				}
			} else {
				faultCode(t, err, Invalid)
			}
		})
	}
}

func TestSetupCalendarDaysFollowDaylightSavingTime(t *testing.T) {
	location := setupLocation(t, "Europe/Berlin")
	for _, tc := range []struct {
		name     string
		month    time.Month
		day      int
		duration time.Duration
	}{
		{name: "spring", month: time.March, day: 29, duration: 23 * time.Hour},
		{name: "autumn", month: time.October, day: 25, duration: 25 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := validSetup(t)
			start := time.Date(2026, tc.month, tc.day, 0, 0, 0, 0, location)
			setCalendarDays(&s, start)
			if duration := s.Days[0].EndsAt.Sub(s.Days[0].StartsAt); duration != tc.duration {
				t.Fatalf("calendar day duration = %v, want %v", duration, tc.duration)
			}
			if err := ValidateSetup(s); err != nil {
				t.Fatalf("valid DST calendar day rejected: %v", err)
			}
		})
	}
}

func setupLocation(t *testing.T, timezone string) *time.Location {
	t.Helper()
	location, err := time.LoadLocation(timezone)
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func setCalendarDays(s *Setup, start time.Time) {
	s.Campaign.Timezone = start.Location().String()
	s.Campaign.StartsAt = start
	s.Campaign.EndsAt = start.AddDate(0, 0, len(s.Days))
	for i := range s.Days {
		setSetupPeriod(s, i, start.AddDate(0, 0, i), start.AddDate(0, 0, i+1))
	}
	for i := range s.Draws {
		if s.Draws[i].Kind == "final" {
			s.Draws[i].ClosesAt = s.Campaign.EndsAt
		}
	}
}

func setSetupPeriod(s *Setup, index int, start, end time.Time) {
	s.Days[index].StartsAt = start
	s.Days[index].EndsAt = end
	for i := range s.Draws {
		if s.Draws[i].ID == s.Days[index].DrawID {
			s.Draws[i].ClosesAt = end
		}
	}
}
