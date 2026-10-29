// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// The prototype notice on the full edge and llm-gw: nothing works before
// consent, a declined notice leaves no account, a new text version asks again,
// and the daily cost cap stops LLM calls at the boundary.
func TestConsentE2E(t *testing.T) {
	s := newStack(t, "127.84")
	s.cfg.CostCapDayEUR = 5
	s.buildEdge(t)
	ctx := context.Background()
	me := &alice
	page := func(method, u string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
		t.Helper()
		if !strings.HasPrefix(u, "https://") {
			u = "https://apps.example.org" + u
		}
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		r := httptest.NewRequest(method, u, body)
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		r.Header.Set("Accept", "text/html")
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		r.RemoteAddr = "10.0.0.2:40000"
		r.Header.Set("X-Authentik-Uid", me.uid)
		r.Header.Set("X-Authentik-Username", me.username)
		r.Header.Set("X-Authentik-Groups", me.groups)
		w := httptest.NewRecorder()
		s.edge.ServeHTTP(w, r)
		return w
	}
	found := func(w *httptest.ResponseRecorder, code int, loc string) {
		t.Helper()
		if w.Code != code || w.Header().Get("Location") != loc {
			t.Fatalf("got %d %q, want %d %q; body:\n%s", w.Code, w.Header().Get("Location"), code, loc, w.Body.String())
		}
	}
	noAccount := func() {
		t.Helper()
		if _, err := s.st.UserByUID(ctx, me.uid); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("account exists before consent: %v", err)
		}
	}

	// gate on both hosts
	found(page("GET", "/projects/new", nil), 302, "https://apps.example.org/consent?next=%2Fprojects%2Fnew")
	found(page("GET", "https://alice.apps.example.org/server/x/session/ses_1", nil, "Sec-Fetch-Mode", "navigate"), 302,
		"https://apps.example.org/consent?next="+url.QueryEscape("https://alice.apps.example.org/server/x/session/ses_1"))
	want(t, page("POST", "/projects/start", url.Values{"description": {"x"}}), http.StatusForbidden)
	w := page("GET", "/consent?next=%2Fprojects%2Fnew", nil)
	want(t, w, http.StatusOK, consent.Text("de").Title, consent.Text("de").Points[0], consent.Text("de").Points[4], "Ich habe das gelesen und bin einverstanden.",
		">Weiter</button>", "Nicht einverstanden – zurück", `href="/consent/declined"`, `value="/projects/new"`, consent.Text("de").Short)
	if strings.Contains(w.Body.String(), "Hauptnavigation") {
		t.Error("the notice page has no main navigation")
	}
	want(t, page("GET", "/privacy", nil), http.StatusOK, consent.Text("de").Title, "Zum Hinweis und zur Zustimmung")
	want(t, page("GET", "/consent/declined", nil), http.StatusOK, "Es wurde nichts gespeichert.", "/outpost.goauthentik.io/sign_out")
	noAccount()

	// CSRF: a cross-site POST never stores consent; an unchecked box is an error
	want(t, page("POST", "/consent", url.Values{"agree": {"yes"}}, "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"),
		http.StatusForbidden)
	want(t, page("POST", "/consent", url.Values{"agree": {"yes"}}, "Sec-Fetch-Site", "same-site"), http.StatusForbidden)
	want(t, page("POST", "/consent", url.Values{"next": {"/"}}), http.StatusUnprocessableEntity, "Bitte bestätige")
	if ok, _ := s.st.HasConsent(ctx, me.uid, consent.Version()); ok {
		t.Fatal("consent stored by a refused request")
	}
	noAccount()
	found(page("POST", "/consent", url.Values{"agree": {"yes"}, "next": {"//evil.example/"}}, "Origin", "https://apps.example.org"),
		303, "/")
	found(page("POST", "/consent", url.Values{"agree": {"yes"}, "next": {"https://alice.apps.example.org/server/x"}}), 303,
		"https://alice.apps.example.org/server/x")
	want(t, page("GET", "/", nil), http.StatusOK, consent.Text("de").Short, `href="/privacy#prototype"`)
	if _, err := s.st.UserByUID(ctx, me.uid); err != nil {
		t.Fatalf("account after consent: %v", err)
	}
	want(t, page("GET", "/help", nil), http.StatusOK, consent.Text("de").Title, "Du hast am ", "zugestimmt")
	want(t, page("GET", "/privacy", nil), http.StatusOK, "Du hast am ", "zugestimmt")
	want(t, page("GET", "/usage", nil), http.StatusOK, "0,00 € von 5,00 € Tagesbudget")

	// llm-gw and Workspace-MCP, wired as in run()
	var upstream int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	uu, _ := url.Parse(up.URL)
	log := slog.New(slog.DiscardHandler)
	mux := http.NewServeMux()
	mux.Handle("/mcp/workspace", mcp.New(mcp.Server{SearXNG: &url.URL{}, SlotOf: s.cfg.SlotOf, Users: s.st, Usage: s.st, Log: log}))
	mux.Handle("/", llm.New(llm.Gateway{Upstream: uu, APIKey: "k", SlotOf: s.cfg.SlotOf, Users: s.st, Usage: s.st, Log: log}))
	gw := gwGate(s.cfg, s.st, budget(s.cfg, s.st), mux)
	gwCall := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://llm-gw:4000"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.RemoteAddr = s.cfg.SlotAddr(1, config.HostUser).String() + ":40000"
		w := httptest.NewRecorder()
		gw.ServeHTTP(w, r)
		return w
	}
	chat := func() *httptest.ResponseRecorder {
		return gwCall("/v1/chat/completions", `{"model":"default"}`)
	}
	mcpList := func() *httptest.ResponseRecorder {
		return gwCall("/mcp/workspace", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	}
	want(t, chat(), http.StatusOK)
	want(t, mcpList(), http.StatusOK, "web_search")

	// new text version: everything asks again
	if err := s.st.AcceptConsent(ctx, me.uid, "alte-fassung", time.Now()); err != nil {
		t.Fatal(err)
	}
	found(page("GET", "/", nil), 302, "https://apps.example.org/consent?next=%2F")
	want(t, page("GET", "/consent/declined", nil), http.StatusOK, "Deine bisherigen Projekte bleiben erhalten")
	if strings.Contains(page("GET", "/consent", nil).Body.String(), "Du hast am") {
		t.Error("an old version is shown as accepted")
	}
	want(t, chat(), http.StatusForbidden, "/consent", "permission_denied")
	want(t, mcpList(), http.StatusForbidden, "/consent")
	before := upstream
	found(page("POST", "/consent", url.Values{"agree": {"yes"}}), 303, "/")
	if upstream != before {
		t.Fatal("refused calls reached LiteLLM")
	}

	// daily cap: yesterday does not count, 4.99 today passes, 5.00 refuses
	spend := func(eur float64, at time.Time) {
		t.Helper()
		id := "ev-" + at.Format(time.RFC3339Nano)
		if err := s.st.InsertUsage(ctx, store.UsageEvent{EventID: id, Start: at, End: at, UID: me.uid, Capability: "llm", Status: 200, CostEUR: &eur}); err != nil {
			t.Fatal(err)
		}
	}
	day := config.DayStart(time.Now())
	spend(100, day.Add(-time.Millisecond))
	spend(4.99, day) // the chats above are metered too, at cost 0 from this upstream
	want(t, chat(), http.StatusOK)
	spend(0.01, day.Add(time.Millisecond))
	want(t, chat(), http.StatusTooManyRequests, "Tagesbudget erreicht, morgen geht es weiter", "insufficient_quota")
	want(t, mcpList(), http.StatusOK) // tools are not LLM spend
	want(t, page("GET", "/usage", nil), http.StatusOK, "5,00 € von 5,00 € Tagesbudget", "Tagesbudget erreicht")
}
