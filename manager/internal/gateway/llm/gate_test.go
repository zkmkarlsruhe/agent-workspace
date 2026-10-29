// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
)

func TestGate(t *testing.T) {
	var passed int
	consented, spent := false, 0.0
	var since time.Time
	now := time.Date(2026, 10, 17, 23, 30, 0, 0, time.UTC) // 01:30 in Berlin, already the 18th
	g := &Gate{SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{},
		Consented: func(_ context.Context, uid string) (bool, error) { return consented && uid == "u1", nil },
		Budget: &Budget{Spent: func(_ context.Context, uid string, s time.Time) (float64, error) {
			since = s
			return spent, nil
		}, CapEUR: 5, Now: func() time.Time { return now }},
		Portal: "https://apps.example.org",
		Next:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { passed++ })}
	do := func(method, path, remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w
	}
	refused := func(w *httptest.ResponseRecorder, code int, typ, msg string) {
		t.Helper()
		var e struct {
			Error struct{ Message, Type string }
		}
		json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != code || e.Error.Type != typ || !strings.Contains(e.Error.Message, msg) || w.Header().Get("x-should-retry") != "false" {
			t.Fatalf("want %d %s %q, got %d %s", code, typ, msg, w.Code, w.Body.String())
		}
	}

	// no consent: LLM and MCP refused before anything else happens
	refused(do("POST", "/v1/chat/completions", "10.90.1.10:1"), 403, "permission_denied", "https://apps.example.org/consent")
	refused(do("POST", "/mcp/workspace", "10.90.1.10:1"), 403, "permission_denied", "Hinweis zum Prototyp")
	// unknown source or person: the inner handler refuses it as before
	do("POST", "/v1/chat/completions", "10.90.2.10:1")
	do("POST", "/v1/chat/completions", "10.90.1.2:1")
	if passed != 2 {
		t.Fatalf("unknown sources must reach Next: %d", passed)
	}

	consented, passed = true, 0
	spent = 4.99
	if do("POST", "/v1/chat/completions", "10.90.1.10:1"); passed != 1 {
		t.Fatal("below the cap must pass")
	}
	if want := time.Date(2026, 10, 18, 0, 0, 0, 0, time.FixedZone("CEST", 2*3600)); !since.Equal(want) {
		t.Fatalf("day start %s, want %s (Berlin midnight)", since, want)
	}
	spent = 5.00
	refused(do("POST", "/v1/chat/completions", "10.90.1.10:1"), 429, "insufficient_quota", "Tagesbudget erreicht, morgen geht es weiter")
	refused(do("POST", "/v1/embeddings", "10.90.1.10:1"), 429, "insufficient_quota", "Tagesbudget")
	// not metered, so not capped: model list and MCP tools
	do("GET", "/v1/models", "10.90.1.10:1")
	do("POST", "/mcp/workspace", "10.90.1.10:1")
	if passed != 3 {
		t.Fatalf("uncapped routes: %d", passed)
	}
	g.Budget.CapEUR, spent = 0, 1e9
	if do("POST", "/v1/chat/completions", "10.90.1.10:1"); passed != 4 {
		t.Fatal("cap 0 means no cap")
	}
}
