// SPDX-License-Identifier: MIT

package portal

import (
	"net/http"
	"os"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
)

// TestHowto: the page renders in both languages before consent and shows
// the limits as configured (defaults = the deployed values: the gitops compose
// sets none of the three), so a changed default changes the page with it.
func TestHowto(t *testing.T) {
	cfg, _ := config.Load(func(key string) string {
		if key == "OC_INSTANCE_FILE" {
			return "../../../instance/example/instance.yaml"
		}
		return ""
	}) // only the defaults are needed
	h, err := New(Config{Backend: newFake(), Assets: os.DirFS("../../web"), Origin: "https://apps.example.org",
		Capabilities: map[string]bool{"image_generate": true}, Consent: &fakeConsent{}, Connections: &fakeConns{}, Tools: &fakeTools{},
		DayCapEUR: cfg.CostCapDayEUR, IdleTimeout: cfg.IdleTimeout, MaxRunning: cfg.MaxRunning})
	if err != nil {
		t.Fatal(err)
	}
	for l, want := range map[string][]string{
		"de": {"So funktioniert das hier", "5,00&nbsp;€", "Nach 20 Minuten ohne Aktivität", "Höchstens 25 Umgebungen",
			"Zum Hinweis und zur Zustimmung", "EU AI Act", "„Agent Workspace“", `href="/tools"`, `id="nextcloud"`},
		"en": {"How this works", "€5.00", "After 20 minutes without activity", "At most 25 environments",
			"To the notice and consent", "EU AI Act", `href="/tools"`, `id="nextcloud"`},
	} {
		w := do(t, h, req{path: "/how-it-works", header: map[string]string{"Cookie": "sprache=" + l}})
		wantStatus(t, w, http.StatusOK)
		wantBody(t, w, want...)
	}
	// linked from the consent notice, the help page and the header
	wantBody(t, do(t, h, req{path: "/consent"}), `href="/how-it-works"`)
	wantBody(t, do(t, h, req{path: "/help"}), `href="/how-it-works"`, "Das Tagesbudget beträgt 5,00\u00a0€", "Nach 20&nbsp;Minuten")
	wantBody(t, do(t, h, req{path: "/environment"}), "nach 20&nbsp;Minuten")
}
