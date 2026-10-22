//go:build integration

package app_test

import (
	"strings"
	"testing"
)

func TestDemoPromoExplainsWhereAndWhenToUseCode(t *testing.T) {
	h := setup(t, "standard")
	reply := h.execute(t, h.command("promo"))
	for _, expected := range []string{
		"Промокод: DEMO-",
		"22.10.2026 00:00 MSK",
		"https://example.org/demo/01",
	} {
		if !strings.Contains(reply.Text, expected) {
			t.Fatalf("promo reply is missing %q: %s", expected, reply.Text)
		}
	}
	if repeated := h.execute(t, h.command("promo")); repeated.Text != reply.Text {
		t.Fatal("repeated request lost the assigned code or application instructions")
	}
}

func TestDemoRulesStatePrizeClaimPeriods(t *testing.T) {
	for _, scenario := range []struct {
		name, dailyPeriod string
	}{
		{"standard", "в течение 24 часов"},
		{"expired", "в течение часа"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h := setup(t, scenario.name)
			reply := h.execute(t, h.command("rules"))
			if !strings.Contains(reply.Text, scenario.dailyPeriod) || !strings.Contains(reply.Text, "финального в течение 48 часов") {
				t.Fatalf("rules do not state the claim periods: %s", reply.Text)
			}
		})
	}
}
