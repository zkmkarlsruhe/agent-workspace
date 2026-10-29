// SPDX-License-Identifier: MIT

package portal

import (
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestCost(t *testing.T) {
	for _, tc := range []struct {
		name, lang string
		turn       *store.UsageRow
		today      store.UsageRow
		spent, cap float64
		want       string
	}{
		{name: "no turn or cap", lang: "en", spent: 1.25, want: "Today: €1.25"},
		{name: "daily cap", lang: "en", spent: 1.25, cap: 5, want: "Today: €1.25 of €5.00"},
		{name: "zero cost", lang: "en", turn: &store.UsageRow{}, want: "This answer: €0.00 · today €0.00"},
		{name: "sub-cent costs", lang: "en", turn: &store.UsageRow{CostEUR: 0.001}, spent: 0.004, want: "This answer: under €0.01 · today under €0.01"},
		{name: "rounding boundary", lang: "en", turn: &store.UsageRow{CostEUR: 0.005}, spent: 0.009, want: "This answer: €0.01 · today €0.01"},
		{name: "German sub-cent", lang: "de", turn: &store.UsageRow{CostEUR: 0.001}, spent: 0.004, cap: 5, want: "Diese Antwort: unter 0,01\u00a0€ · heute unter 0,01\u00a0€ von 5,00\u00a0€"},
		{name: "daily emissions", lang: "en", today: store.UsageRow{CO2LoG: 1, CO2HiG: 2}, want: "Today: €0.00 · approx. 1–2\u00a0g CO2"},
		{name: "turn emissions", lang: "en", turn: &store.UsageRow{CO2LoG: 0.1, CO2HiG: 0.2}, today: store.UsageRow{CO2HiG: 9}, want: "This answer: €0.00 · approx. 0.1–0.2\u00a0g CO2 · today €0.00"},
		{name: "fallback language", lang: "fr", want: "Heute: 0,00\u00a0€"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Cost(tc.lang, tc.turn, tc.today, tc.spent, tc.cap); got != tc.want {
				t.Fatalf("Cost = %q, want %q", got, tc.want)
			}
		})
	}
}
