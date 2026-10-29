// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// GET /workspace/usage: the numbers of the person of the source address only,
// one answer = the chat's usage since "since", subagents included.
func TestUsage(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	me, _ := st.EnsureUser(ctx, identity.Identity{UID: fmt.Sprintf("%064x", 1), Username: "alice"})
	other, _ := st.EnsureUser(ctx, identity.Identity{UID: fmt.Sprintf("%064x", 2), Username: "bob"})
	now := config.DayStart(time.Now()).Add(12 * time.Hour) // midday: all events of one day
	f := func(v float64) *float64 { return &v }
	for i, e := range []store.UsageEvent{
		{UID: me.UID, SessionID: "ses_aaaaaaaaaa", RootSessionID: "ses_aaaaaaaaaa", CostEUR: f(0.30), CO2LoG: f(1), CO2HiG: f(3), Start: now.Add(-2 * time.Minute)},
		{UID: me.UID, SessionID: "ses_aaaaaaaaaa", RootSessionID: "ses_aaaaaaaaaa", CostEUR: f(0.015), CO2LoG: f(0.4), CO2HiG: f(1.2), Start: now},
		{UID: me.UID, SessionID: "ses_subsubsub1", RootSessionID: "ses_aaaaaaaaaa", CostEUR: f(0.005), CO2LoG: f(0.1), CO2HiG: f(0.3), Start: now},
		{UID: other.UID, SessionID: "ses_aaaaaaaaaa", RootSessionID: "ses_aaaaaaaaaa", CostEUR: f(9), Start: now},
	} {
		e.EventID, e.Capability, e.Status, e.End = fmt.Sprint(i), "llm", 200, e.Start
		if err := st.InsertUsage(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	slots := map[string]int{"10.90.1.10": me.Slot, "10.90.2.10": other.Slot}
	v := usageHandler{st: st, capEUR: 5, now: func() time.Time { return now },
		slotOf: func(a netip.Addr) (int, bool) { s, ok := slots[a.String()]; return s, ok }}
	get := func(from, query string) (int, usageJSON) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/workspace/usage"+query, nil)
		r.RemoteAddr = from + ":40000"
		w := httptest.NewRecorder()
		v.ServeHTTP(w, r)
		var out usageJSON
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return w.Code, out
	}
	since := fmt.Sprintf("&since=%d", now.Add(-time.Minute).UnixMilli())

	code, out := get("10.90.1.10", "?session=ses_aaaaaaaaaa"+since)
	if code != 200 || out.AnswerEUR == nil || *out.AnswerEUR != 0.02 || out.BudgetEUR != 5 {
		t.Fatalf("%d %+v", code, out)
	}
	// the answer before since counts for today only
	if want := "Diese Antwort: 0,02 € · ca. 0,5–2 g CO2 · heute 0,32 € von 5,00 €"; out.Text != want {
		t.Errorf("text %q, want %q", out.Text, want)
	}
	if code, out = get("10.90.1.10", ""); code != 200 || out.AnswerEUR != nil || !strings.HasPrefix(out.Text, "Heute: 0,32\u00a0€ von 5,00\u00a0€ · ca. 2–5\u00a0g CO2") {
		t.Fatalf("today only: %d %+v", code, out)
	}
	st.SetLang(ctx, me.UID, "en")
	if code, out = get("10.90.1.10", "?session=ses_aaaaaaaaaa"+since); code != 200 || !strings.HasPrefix(out.Text, "This answer: €0.02 · approx. 0.5–2\u00a0g CO2 · today €0.32 of €5.00") {
		t.Fatalf("en: %d %q", code, out.Text)
	}
	// bob's numbers for the same session id, not alice's
	if code, out = get("10.90.2.10", "?session=ses_aaaaaaaaaa"+since); code != 200 || *out.AnswerEUR != 9 {
		t.Fatalf("other: %d %+v", code, out)
	}
	for _, c := range []struct {
		from, q string
		code    int
	}{
		{"10.90.1.10", fmt.Sprintf("?session=ses_aaaaaaaaaa&since=%d", now.Add(time.Minute).UnixMilli()), 204}, // nothing metered yet
		{"10.90.1.10", "?session=ses_aaaaaaaaaa", 400},
		{"10.90.1.10", "?session=x'or'1&since=0", 400},
		{"10.90.9.10", "", 403},
	} {
		if code, _ := get(c.from, c.q); code != c.code {
			t.Errorf("%s %s: %d, want %d", c.from, c.q, code, c.code)
		}
	}
}
