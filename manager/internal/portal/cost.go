// SPDX-License-Identifier: MIT

package portal

import "github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"

// Cost is the row the workspace-kosten plugin shows in the chat (GET /workspace/usage
// on llm-gw): the answer's cost and CO2 if turn is set, else today's CO2, and
// today's spend against the daily cap (capEUR <= 0: no cap).
func Cost(lang string, turn *store.UsageRow, today store.UsageRow, spentEUR, capEUR float64) string {
	if lang != langEN {
		lang = langDE
	}
	t := func(de string) string { return tr(lang, de) }
	money := func(v float64) string {
		if v > 0 && v < 0.005 {
			return t("under") + " " + eurL(lang, 0.01)
		}
		return eurL(lang, v)
	}
	co2 := func(u store.UsageRow) string {
		if u.CO2HiG <= 0 {
			return ""
		}
		return " · " + t("approx.") + " " + num(lang, co2Range(u.CO2LoG, u.CO2HiG)) + " CO2"
	}
	day := money(spentEUR)
	if capEUR > 0 {
		day += " " + t("of") + " " + eurL(lang, capEUR)
	}
	if turn == nil {
		return t("Today") + ": " + day + co2(today)
	}
	return t("This answer") + ": " + money(turn.CostEUR) + co2(*turn) + " · " + t("today") + " " + day
}
