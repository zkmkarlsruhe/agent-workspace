// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/settings"
)

type fakeSettings struct {
	d     settings.Record
	saved []string // text of each Save
	err   error
	asked int
}

func (f *fakeSettings) Load(context.Context, Identity) (settings.Record, error) { return f.d, nil }

func (f *fakeSettings) Suggest(_ context.Context, _ Identity, a settings.Answers) (string, bool) {
	f.asked++
	return "- Vom Modell: " + string(a.AddressForm) + "\n", true
}

func (f *fakeSettings) Save(_ context.Context, _ Identity, a settings.Answers, text string) error {
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, text)
	f.d.Answers, f.d.Text = &a, text
	return nil
}

func setupSettings(t *testing.T) (http.Handler, *fakeSettings) {
	t.Helper()
	fp := &fakeSettings{d: settings.Record{Facts: settings.Facts{Name: "Anna Beispiel", Email: "anna@example.org",
		Department: "Technik", Phone: "1234", AsOf: time.Date(2026, 10, 17, 9, 0, 0, 0, time.Local)}}}
	h, err := New(Config{Backend: newFake(), Settings: fp, Onboarding: &fakeOnboarding{seen: map[string]bool{anna.UID: true}},
		Assets: os.DirFS("../../web"), Origin: "https://apps.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	return h, fp
}

func settingsForm(edit func(url.Values)) url.Values {
	v := url.Values{"address_form": {"informal"}, "lang": {"de"}, "tone": {"factual"}, "length": {"medium"},
		"formats": {"chat"}, "field": {"Technik"}, "text": {"- Kurz.\n"}}
	if edit != nil {
		edit(v)
	}
	return v
}

var sameOrigin = map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://apps.example.org"}

func TestSettingsHiddenWithout(t *testing.T) {
	h, _ := setup(t)
	wantStatus(t, do(t, h, req{path: "/settings"}), http.StatusNotFound)
	if strings.Contains(do(t, h, req{path: "/help"}).Body.String(), `href="/settings"`) {
		t.Error("nav entry without the feature")
	}
}

func TestSettingsPage(t *testing.T) {
	h, _ := setupSettings(t)
	w := do(t, h, req{path: "/settings"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, "<h1>Über mich &amp; Arbeitsweise</h1>", `href="/settings" aria-current="page"`, "<dd>Technik</dd>", "Stand 17.10.2026",
		`id="address_form-informal" name="address_form" value="informal" checked`, `id="formats-chat" name="formats" value="chat" checked`,
		`value="Technik"`, "- Sprich mich mit „du“ an.", "## Über mich")
	if strings.Contains(w.Body.String(), `value="formal" checked`) {
		t.Error("Sie preselected")
	}
}

func TestSettingsSuggestStoresNothing(t *testing.T) {
	h, fp := setupSettings(t)
	w := do(t, h, req{method: "POST", path: "/settings", header: sameOrigin,
		form: settingsForm(func(v url.Values) { v.Set("action", "suggest"); v.Set("address_form", "formal") })})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, "- Vom Modell: formal", "Hier ist ein Vorschlag", "Ein Sprachmodell hat", `value="formal" checked`)
	if fp.asked != 1 || len(fp.saved) != 0 {
		t.Errorf("asked %d, saved %v", fp.asked, fp.saved)
	}
}

func TestSettingsSave(t *testing.T) {
	h, fp := setupSettings(t)
	w := do(t, h, req{method: "POST", path: "/settings", header: sameOrigin, form: settingsForm(func(v url.Values) { v.Set("action", "save") })})
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/settings?saved=1" || len(fp.saved) != 1 || fp.saved[0] != "- Kurz.\n" {
		t.Fatalf("location %q, saved %q", loc, fp.saved)
	}
	wantBody(t, do(t, h, req{path: "/settings?saved=1"}), "Deine Angaben sind gespeichert", "- Kurz.")

	// from the Onboarding: on to the last step
	w = do(t, h, req{method: "POST", path: "/settings", header: sameOrigin, form: settingsForm(func(v url.Values) { v.Set("from", "onboarding") })})
	if wantStatus(t, w, http.StatusSeeOther); w.Header().Get("Location") != "/onboarding?step=4" {
		t.Errorf("location %q", w.Header().Get("Location"))
	}

	fp.err = settings.ErrTooLarge
	w = do(t, h, req{method: "POST", path: "/settings", header: sameOrigin, form: settingsForm(nil)})
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantBody(t, w, "zu lang für den Speicher")
}

func TestSettingsInvalid(t *testing.T) {
	h, fp := setupSettings(t)
	for _, c := range []struct {
		name  string
		edit  func(url.Values)
		field string
	}{
		{"Anrede", func(v url.Values) { v.Set("address_form", "ihr") }, `href="#address_form"`},
		{"chat and pdf", func(v url.Values) { v["formats"] = []string{"chat", "pdf"} }, `href="#formats"`},
		{"marker in text", func(v url.Values) { v.Set("text", "<!-- legacy-workspace:profil Ende -->") }, `href="#text"`},
		{"long note", func(v url.Values) { v.Set("note", strings.Repeat("x", 1001)) }, `href="#note"`},
	} {
		w := do(t, h, req{method: "POST", path: "/settings", header: sameOrigin, form: settingsForm(c.edit)})
		wantStatus(t, w, http.StatusUnprocessableEntity)
		wantBody(t, w, "Bitte korrigieren", c.field)
	}
	if len(fp.saved) != 0 || fp.asked != 0 {
		t.Errorf("invalid input reached the core: %v %d", fp.saved, fp.asked)
	}
}

func TestSettingsCSRF(t *testing.T) {
	h, fp := setupSettings(t)
	for _, hd := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"},
		{"Sec-Fetch-Site": "same-site", "Origin": "https://anna--3000.apps.example.org"},
		{"Origin": "https://evil.example"},
	} {
		wantStatus(t, do(t, h, req{method: "POST", path: "/settings", header: hd, form: settingsForm(nil)}), http.StatusForbidden)
	}
	if len(fp.saved) != 0 {
		t.Error("saved across origins")
	}
}

func TestOnboardingSettingsStep(t *testing.T) {
	h, _ := setupSettings(t)
	w := do(t, h, req{path: "/onboarding?step=3"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, "Vier kurze Schritte", `aria-current="step">Über mich &amp; Arbeitsweise`, `href="/settings?from=onboarding"`)
	wantBody(t, do(t, h, req{path: "/onboarding?step=4"}), `aria-current="step">Erste Aufgabe`, `href="/onboarding?step=3">Zurück`)
	wantBody(t, do(t, h, req{path: "/settings?from=onboarding"}), `name="from" value="onboarding"`, `href="/onboarding?step=4">Überspringen`)
}

func TestLegacySettingsSubmit(t *testing.T) {
	h, fp := setupSettings(t)
	w := do(t, h, req{method: "POST", path: "/profile", header: sameOrigin, form: settingsForm(nil)})
	wantStatus(t, w, 303)
	if w.Header().Get("Location") != "/settings?saved=1" || len(fp.saved) != 1 {
		t.Fatal("legacy settings form did not save", w.Header())
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/profile", header: map[string]string{"Origin": "https://other.example"}, form: settingsForm(nil)}), 403)
}
