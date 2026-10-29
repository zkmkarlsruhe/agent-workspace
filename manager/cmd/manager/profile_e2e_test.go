// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/settings"
)

type dirStub struct{ e settings.Entry }

func (d dirStub) Lookup(context.Context, string, string) (settings.Entry, error) { return d.e, nil }

// withSettings wires the personal settings like run() and maps AGENTS.md to a temp file.
func withSettings(t testing.TB, s *stack) string {
	agents := filepath.Join(t.TempDir(), "AGENTS.md")
	s.rt.Paths[settings.AgentsPath] = agents
	portalSettings = newSettings(s.cfg, s.st, s.svc, nil, slog.New(slog.DiscardHandler))
	portalSettings.Dir = dirStub{settings.Entry{Department: "Technik", Role: "Leitung", Phone: "+49 30 1234-1234", Room: "B 2.13"}}
	t.Cleanup(func() { portalSettings = nil })
	s.buildEdge(t)
	return agents
}

func TestSettingsE2E(t *testing.T) {
	s := newStack(t, "127.87")
	agents := withSettings(t, s)
	os.WriteFile(agents, []byte("## Gemerkt\n- Kaffee schwarz.\n"), 0o644)

	w := s.do(t, call{url: "/settings", as: &alice})
	want(t, w, 200, "<h1>Über mich &amp; Arbeitsweise</h1>", "<dd>Technik</dd>", `value="informal" checked`, "Technik, Leitung")

	form := url.Values{"address_form": {"formal"}, "lang": {"de"}, "tone": {"friendly"}, "length": {"short"},
		"formats": {"pdf", "excel"}, "always": {"Quellen nennen"}, "action": {"suggest"}}
	w = s.do(t, call{method: "POST", url: "/settings", form: form, as: &alice})
	want(t, w, 200, "Hier ist ein Vorschlag", "- Sprich mich mit „Sie“ an.", "- Immer: Quellen nennen", "als PDF oder Excel.")

	// save while asleep: the block arrives with the next start, "Gemerkt" stays
	form.Set("action", "save")
	form.Set("text", "- Sprich mich mit „Sie“ an.\n- Kurz bitte.\n")
	wantLoc(t, s.do(t, call{method: "POST", url: "/settings", form: form, as: &alice}), "/settings?saved=1")
	if b, _ := os.ReadFile(agents); strings.Contains(string(b), "So arbeite ich") {
		t.Fatal("written although the environment sleeps")
	}
	if err := s.svc.EnvStart(context.Background(), alice.uid); err != nil {
		t.Fatal(err)
	}
	s.waitRunning(t)
	var got string
	for range 100 { // AfterStart runs right after the container counts as running
		if b, _ := os.ReadFile(agents); strings.Contains(string(b), "So arbeite ich") {
			got = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, sub := range []string{"## Über mich\n", "- Abteilung: Technik\n", "- Raum: B 2.13\n",
		"## So arbeite ich\n- Sprich mich mit „Sie“ an.\n- Kurz bitte.\n", "\n## Gemerkt\n- Kaffee schwarz.\n"} {
		if !strings.Contains(got, sub) {
			t.Errorf("AGENTS.md lacks %q:\n%s", sub, got)
		}
	}
	if strings.Contains(got, "opencode-users") {
		t.Error("access group listed as a fact")
	}

	// running: a save lands at once, in place
	form.Set("text", "- Ganz kurz.\n")
	wantLoc(t, s.do(t, call{method: "POST", url: "/settings", form: form, as: &alice}), "/settings?saved=1")
	b, _ := os.ReadFile(agents)
	if strings.Count(string(b), "## So arbeite ich") != 1 || !strings.Contains(string(b), "- Ganz kurz.\n") || !strings.HasSuffix(string(b), "- Kaffee schwarz.\n") {
		t.Errorf("after second save:\n%s", b)
	}

	// language switch: the block is rewritten in English at once; own text and "Gemerkt" stay
	wantLoc(t, s.do(t, call{method: "POST", url: "/lang", form: url.Values{"lang": {"en"}, "next": {"/settings"}}, as: &alice}), "/settings")
	b, _ = os.ReadFile(agents)
	for _, sub := range []string{"## About me\n", "- Department: Technik\n", "- Room: B 2.13\n", "## How I work\n- Ganz kurz.\n", "- Kaffee schwarz.\n"} {
		if !strings.Contains(string(b), sub) {
			t.Errorf("English AGENTS.md lacks %q:\n%s", sub, b)
		}
	}
	if strings.Contains(string(b), "Über mich") || strings.Count(string(b), "agent-workspace:profile end") != 1 {
		t.Errorf("German block left or block doubled:\n%s", b)
	}
}
