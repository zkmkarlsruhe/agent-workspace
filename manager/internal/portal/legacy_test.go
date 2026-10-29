// SPDX-License-Identifier: MIT

package portal

import (
	"net/http"
	"testing"
)

func TestLegacyRedirects(t *testing.T) {
	h, _ := setup(t)
	for from, to := range map[string]string{
		"/profiles?profile=it-hilfe":           "/assistants?profile=it-hilfe",
		"/profile?from=onboarding":             "/settings?from=onboarding",
		"/profil?saved=1":                      "/settings?saved=1",
		"/umgebung":                            "/environment",
		"/projekte/neu?vorlage=table":          "/projects/new?template=table",
		"/projekte/p/zip?teil=alles":           "/projects/p/zip?part=all",
		"/projekte/p/dateien/results/a%20b.md": "/projects/p/files/results/a%20b.md",
		"/zustimmung?weiter=%2Fhilfe":          "/consent?next=%2Fhilfe",
		"/werkzeuge/eigene/meine-x/loeschen":   "/tools/own/meine-x/delete",
	} {
		w := do(t, h, req{path: from})
		if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != to {
			t.Errorf("%s: %d %q, want 301 %q", from, w.Code, w.Header().Get("Location"), to)
		}
	}
}

func TestLegacyProjectAssistantLink(t *testing.T) {
	h, _ := setup(t)
	for _, query := range []string{"profile=it-hilfe", "assistant=it-hilfe", "assistant=it-hilfe&profile=unknown"} {
		w := do(t, h, req{path: "/projects/new?" + query})
		wantStatus(t, w, http.StatusSeeOther)
		if got := w.Header().Get("Location"); got != "/new?assistant=it-hilfe" {
			t.Fatalf("%s: redirect %q", query, got)
		}
	}
}
