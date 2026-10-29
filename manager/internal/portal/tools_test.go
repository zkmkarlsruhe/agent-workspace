// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
)

type fakeTools struct {
	d       ToolsPage
	effect  ToolEffect
	deleted []string
}

func (f *fakeTools) Tools(context.Context, string) (ToolsPage, error) { return f.d, nil }

func (f *fakeTools) SetTool(_ context.Context, _, id string, on bool) (ToolEffect, error) {
	for i, t := range f.d.Catalog {
		if t.ID == id {
			f.d.Catalog[i].On = on
			return f.effect, nil
		}
	}
	return 0, ErrNotFound
}

func (f *fakeTools) DeleteSkill(_ context.Context, _, id string) (ToolEffect, error) {
	f.deleted = append(f.deleted, id)
	return f.effect, nil
}

func setupTools(t *testing.T) (http.Handler, *fakeTools) {
	t.Helper()
	ft := &fakeTools{d: ToolsPage{
		Running:  true,
		Standard: []Tool{{ID: "xlsx", Title: "Excel-Tabellen", Description: "Excel lesen und schreiben.", TitleEN: "Excel spreadsheets"}},
		Catalog: []Tool{{ID: "als-pdf", Title: "Ein PDF erstellen", Description: "Macht PDFs.", Category: "Texte",
			TitleEN: "Create a PDF", DescriptionEN: "Makes PDFs."}},
		Own: []OwnSkill{{ID: "meine-besucher", Description: "Besucherzahlen.", Changed: time.Now()}},
	}}
	h, err := New(Config{Backend: newFake(), Tools: ft, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	return h, ft
}

func TestToolsHiddenWithoutTools(t *testing.T) {
	h, _ := setup(t)
	wantStatus(t, do(t, h, req{path: "/tools"}), http.StatusNotFound)
	if strings.Contains(do(t, h, req{path: "/help"}).Body.String(), `href="/tools"`) {
		t.Error("nav entry without catalog")
	}
}

func TestToolsSwitch(t *testing.T) {
	h, ft := setupTools(t)
	w := do(t, h, req{path: "/tools"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, "Immer dabei", "Excel-Tabellen", "Zusätzliche Fähigkeiten", "Ein PDF erstellen", "ausgeschaltet",
		`action="/tools/als-pdf"`, `name="on" value="1"`, "Einschalten", "Gespeicherte Anleitungen", "meine-besucher",
		`href="/tools/own/meine-besucher/delete"`, `href="/tools" aria-current="page"`)
	if n := strings.Count(w.Body.String(), "<h1"); n != 1 {
		t.Errorf("%d h1", n)
	}

	for eff, text := range map[ToolEffect]string{EffectNow: "ab deinem nächsten Gespräch", EffectBusy: "Die Änderung ist gespeichert", EffectNextStart: "Dein Arbeitsbereich ist ausgeschaltet"} {
		ft.effect = eff
		w = do(t, h, req{method: "POST", path: "/tools/als-pdf", form: url.Values{"on": {"1"}}})
		wantStatus(t, w, http.StatusSeeOther)
		// a browser never sends the fragment
		loc, _, _ := strings.Cut(w.Header().Get("Location"), "#")
		w = do(t, h, req{path: loc})
		wantBody(t, w, "„Ein PDF erstellen“ ist eingeschaltet.", text, `name="on" value="0"`, "Ausschalten")
	}
	w = do(t, h, req{method: "POST", path: "/tools/als-pdf", form: url.Values{"on": {"0"}}})
	if !strings.HasPrefix(w.Header().Get("Location"), "/tools?toggled=als-pdf") || ft.d.Catalog[0].On {
		t.Fatalf("off: %v %+v", w.Header(), ft.d.Catalog)
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/tools/xlsx", form: url.Values{"on": {"1"}}}), http.StatusNotFound)
	// cross-site form posts are refused before anything switches
	w = do(t, h, req{method: "POST", path: "/tools/als-pdf", form: url.Values{"on": {"1"}}, header: map[string]string{"Origin": "https://evil.example"}})
	wantStatus(t, w, http.StatusForbidden)
	if ft.d.Catalog[0].On {
		t.Fatal("switched by a foreign origin")
	}
}

func TestToolsDeleteOwn(t *testing.T) {
	h, ft := setupTools(t)
	wantStatus(t, do(t, h, req{path: "/tools/own/gibtsnicht/delete"}), http.StatusNotFound)
	w := do(t, h, req{path: "/tools/own/meine-besucher/delete"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, "Diese Anleitung löschen?", "nicht rückgängig", `name="confirmed" value="yes"`)
	wantStatus(t, do(t, h, req{method: "POST", path: "/tools/own/meine-besucher/delete"}), http.StatusUnprocessableEntity)
	if len(ft.deleted) != 0 {
		t.Fatal("deleted without confirmation")
	}
	w = do(t, h, req{method: "POST", path: "/tools/own/meine-besucher/delete", form: url.Values{"confirmed": {"yes"}}})
	wantStatus(t, w, http.StatusSeeOther)
	if len(ft.deleted) != 1 || ft.deleted[0] != "meine-besucher" {
		t.Fatalf("deleted %v", ft.deleted)
	}
	loc, _, _ := strings.Cut(w.Header().Get("Location"), "#")
	wantBody(t, do(t, h, req{path: loc}), "„meine-besucher“ wurde gelöscht")

	ft.d.Running = false
	w = do(t, h, req{path: "/tools"})
	wantBody(t, w, "Dein Arbeitsbereich ist ausgeschaltet", `action="/environment/start"`)
	if strings.Contains(w.Body.String(), "/tools/own/") {
		t.Error("delete link while asleep")
	}
	wantStatus(t, do(t, h, req{path: "/tools/own/meine-besucher/delete"}), http.StatusServiceUnavailable)
}

func TestToolsEnglish(t *testing.T) {
	h, _ := setupTools(t)
	en := map[string]string{"Cookie": "sprache=en"}
	w := do(t, h, req{path: "/tools", header: en})
	wantBody(t, w, "Excel spreadsheets", "Excel lesen und schreiben.", "Create a PDF", "Makes PDFs.", "Texts")
	if strings.Contains(w.Body.String(), "Ein PDF erstellen") {
		t.Error("German title on the English page")
	}
	do(t, h, req{method: "POST", path: "/tools/als-pdf", form: url.Values{"on": {"1"}}})
	wantBody(t, do(t, h, req{path: "/tools?toggled=als-pdf&effect=jetzt", header: en}), "“Create a PDF” is switched on.")
	// the English texts never leak into the German page
	wantBody(t, do(t, h, req{path: "/tools"}), "Ein PDF erstellen", "Macht PDFs.", "Texte")
}

// TestCatalogEnglish: every item of the real catalog and every base skill has
// its English title and description, English trigger words for the model,
// and its category has an English label.
func TestCatalogEnglish(t *testing.T) {
	items, err := catalog.Load("../../../deploy/catalog")
	if err != nil || len(items) < 5 {
		t.Fatalf("%d items, %v", len(items), err)
	}
	for _, it := range items {
		if it.Title == "" || it.Description == "" || categoryEN[it.Category] == "" {
			t.Errorf("%s: English %q %q, category %q", it.ID, it.Title, it.Description, it.Category)
		}
	}
	base, _ := filepath.Glob("../../../deploy/etc-opencode/skills/*/SKILL.md")
	cat, _ := filepath.Glob("../../../deploy/catalog/skills/*/SKILL.md")
	base = append(base, cat...)
	if len(base) < 10 {
		t.Fatalf("%d base skills", len(base))
	}
	for _, f := range base {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fm, _ := catalog.ParseFrontmatter(string(b))
		if fm.Meta["workspace/title"] == "" || fm.Meta["workspace/title-de"] == "" || fm.Meta["workspace/description-de"] == "" || !strings.Contains(fm.Description, "Load for ") {
			t.Errorf("%s: no English display texts or trigger words", f)
		}
	}
}
