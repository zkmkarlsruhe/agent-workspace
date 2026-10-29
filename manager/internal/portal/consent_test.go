// SPDX-License-Identifier: MIT

package portal

import (
	"net/http"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
)

func TestResume(t *testing.T) {
	s := &Server{origin: "https://apps.example.org"}
	for in, want := range map[string]string{
		"":                      "/",
		"/projects/new?x=1":     "/projects/new?x=1",
		"//evil.example/":       "/",
		"/\\evil.example/":      "/",
		"/\t/evil.example/":     "/",
		"/consent?next=/":       "/",
		"https://evil.example/": "/",
		"https://alice.apps.example.org/server/x":     "https://alice.apps.example.org/server/x",
		"https://a.b.apps.example.org/":               "/",
		"https://alice.apps.example.org:444/":         "/",
		"https://u@alice.apps.example.org/":           "/",
		"http://alice.apps.example.org/":              "/",
		"https://apps.example.org.evil.example/":      "/",
		"javascript:alert(1)":                         "/",
		"https://alice.apps.example.org/?a=b#session": "https://alice.apps.example.org/?a=b#session",
	} {
		if got := s.next(in); got != want {
			t.Errorf("weiter(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBannerAndFull(t *testing.T) {
	h, f := setup(t)
	f.addProject("demo", "Beschreibung")
	for _, p := range []string{"/", "/environment", "/usage", "/help", "/privacy", "/trash"} {
		w := do(t, h, req{path: p})
		wantStatus(t, w, http.StatusOK)
		wantBody(t, w, consent.Text("de").Short, `href="/privacy#prototype"`)
	}
	wantBody(t, do(t, h, req{path: "/privacy"}), consent.Text("de").Title, consent.Text("de").Points[1], `id="prototype"`)
	wantBody(t, do(t, h, req{path: "/help"}), consent.Text("de").Title, consent.Text("de").Points[2])

	f.env = EnvStatus{State: EnvFull}
	wantBody(t, do(t, h, req{path: "/"}), "Gerade sind alle Plätze belegt, bitte in ein paar Minuten nochmal.")
	w := do(t, h, req{path: "/environment/starting"})
	wantStatus(t, w, http.StatusServiceUnavailable)
	wantBody(t, w, "Gerade sind alle Plätze belegt, bitte in ein paar Minuten nochmal.", "Nochmal versuchen")
	wantBody(t, do(t, h, req{path: "/environment"}), "alle Plätze belegt")
	// a synchronous start (file operations) maps ErrFull to the same text
	f.exportErr = ErrFull
	w = do(t, h, req{path: "/projects/demo/export"})
	wantStatus(t, w, http.StatusServiceUnavailable)
	wantBody(t, w, "Gerade sind alle Plätze belegt")
}
