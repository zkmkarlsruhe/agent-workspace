// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/portal"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// verbrauch answers GET /workspace/usage on llm-gw for the workspace-cost plugin
// (deploy/etc-opencode/plugins). The person is the one of the source network,
// as for /mcp/workspace, so a container only ever reads its own numbers.
// ?session=<root session>&since=<unix ms> adds the cost of that chat since then
// (one answer); 204 when nothing of it is metered yet.
type usageHandler struct {
	slotOf func(netip.Addr) (int, bool)
	st     *store.Store
	capEUR float64
	now    func() time.Time
}

type usageJSON struct {
	Text      string   `json:"text"`
	TodayEUR  float64  `json:"today_eur"`
	BudgetEUR float64  `json:"budget_eur"`
	AnswerEUR *float64 `json:"answer_eur,omitempty"`
}

func (v usageHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	addr, _ := netip.ParseAddr(host)
	slot, ok := v.slotOf(addr)
	var u store.User
	var err error
	if ok {
		u, err = v.st.UserBySlot(ctx, slot)
	}
	if !ok || err != nil {
		http.Error(w, "Kein Zugang.", http.StatusForbidden)
		return
	}
	var turn *store.UsageRow
	if s := r.URL.Query().Get("session"); s != "" {
		since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		if !store.ValidSessionID(s) || err != nil {
			http.Error(w, "session oder since fehlt.", http.StatusBadRequest)
			return
		}
		c, err := v.st.ChatUsage(ctx, u.UID, s, time.UnixMilli(since))
		if err != nil {
			http.Error(w, "Interner Fehler.", http.StatusInternalServerError)
			return
		}
		if c.Sessions == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		turn = &c
	}
	day := config.DayStart(v.now())
	spent, err := v.st.SpentSince(ctx, u.UID, day)
	if err != nil {
		http.Error(w, "Interner Fehler.", http.StatusInternalServerError)
		return
	}
	today, err := v.st.UsageTotal(ctx, u.UID, day)
	if err != nil {
		http.Error(w, "Interner Fehler.", http.StatusInternalServerError)
		return
	}
	lang, _ := v.st.Lang(ctx, u.UID) // "" = German
	out := usageJSON{Text: portal.Cost(lang, turn, today, spent, v.capEUR), TodayEUR: spent, BudgetEUR: v.capEUR}
	if turn != nil {
		out.AnswerEUR = &turn.CostEUR
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}
