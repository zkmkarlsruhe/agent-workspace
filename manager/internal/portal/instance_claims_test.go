// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"html"
	"os"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func TestInstanceProviderClaims(t *testing.T) {
	for _, name := range []string{"minimal", "example"} {
		t.Run(name, func(t *testing.T) {
			d, err := instance.Load("../../../instance/" + name + "/instance.yaml")
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Select(d)()
			h, err := New(Config{Backend: newFake(), Assets: os.DirFS("../../web")})
			if err != nil {
				t.Fatal(err)
			}
			for _, lang := range []string{"en", "de"} {
				for _, path := range []string{"/how-it-works", "/help", "/usage/method"} {
					w := do(t, h, req{path: path, header: map[string]string{"Accept-Language": lang}})
					wantStatus(t, w, 200)
					body := html.UnescapeString(w.Body.String())
					if name == "minimal" {
						for _, claim := range []string{"Qwen", "Mistral", "DeepSeek", "FLUX", "GWDG", "IONOS", "OVHcloud", "No provider from the USA", "Kein Anbieter aus den USA", "Fallback:"} {
							if strings.Contains(body, claim) {
								t.Errorf("%s %s: unexpected %q", lang, path, claim)
							}
						}
					}
					if path == "/how-it-works" {
						if !strings.Contains(body, d.CopyText("providers", lang)) {
							t.Errorf("%s: missing provider copy", lang)
						}
						sentence := "Qwen3.5-397B at IONOS in Berlin, for most tasks. Fallback: the same model at OVHcloud (France), then at GWDG in Göttingen."
						if lang == "de" {
							sentence = "Qwen3.5-397B bei IONOS in Berlin, für die meisten Aufgaben. Ersatz: dasselbe Modell bei OVHcloud (Frankreich), dann bei der GWDG in Göttingen."
						}
						if strings.Contains(body, sentence) != (name == "example") {
							t.Errorf("%s: example model sentence mismatch", lang)
						}
					}
				}
			}
		})
	}
}

func TestHelpCapabilityClaims(t *testing.T) {
	for _, name := range []string{"minimal", "example"} {
		t.Run(name, func(t *testing.T) {
			d, err := instance.Load("../../../instance/" + name + "/instance.yaml")
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Select(d)()
			// Even a stale catalog must not advertise disabled integrations.
			ft := &fakeTools{d: ToolsPage{ProjectScoped: true}}
			for _, id := range []string{"web-research", "md-to-pdf", "workspace-knowledge", "library", "rooms", "media"} {
				ft.d.Catalog = append(ft.d.Catalog, Tool{ID: id})
			}
			h, err := New(Config{Backend: newFake(), Assets: os.DirFS("../../web"), Tools: ft, Capabilities: map[string]bool{"web": true, "image_view": true, "image_generate": true, "image_edit": true, "rooms": true}, Feedback: &fakeFeedback{},
				PDF: func(context.Context, []byte, string) ([]byte, error) { return nil, nil }})
			if err != nil {
				t.Fatal(err)
			}
			for _, lang := range []string{"en", "de"} {
				w := do(t, h, req{path: "/how-it-works", header: map[string]string{"Accept-Language": lang}})
				wantStatus(t, w, 200)
				body := html.UnescapeString(w.Body.String())
				for key, owner := range map[string]string{
					"Search and read the web": "web-research", "PDF from Markdown": "pdf",
					"Look at an image": "images", "Create an image": "images",
					"Library (OPAC)": "library", "Search [[organization]] knowledge": "workspace-knowledge",
					"Feedback for [[recipient]]": "feedback",
				} {
					want := tr(lang, key)
					if strings.Contains(body, want) != d.Enabled(owner) {
						t.Errorf("%s %s: presence of %q", name, lang, want)
					}
				}
				w = do(t, h, req{path: "/help", header: map[string]string{"Accept-Language": lang}})
				body = html.UnescapeString(w.Body.String())
				label := "As PDF"
				if lang == "de" {
					label = "Als PDF"
				}
				if strings.Contains(body, label) != (name == "example") {
					t.Errorf("%s %s: PDF help mismatch", name, lang)
				}
			}
			// Removing catalog entries removes their descriptions even if integrations stay enabled.
			ft.d.Catalog = nil
			w := do(t, h, req{path: "/how-it-works", header: map[string]string{"Accept-Language": "en"}})
			for _, claim := range []string{"Search and read the web", "PDF from Markdown", "Library (OPAC)"} {
				if strings.Contains(w.Body.String(), claim) {
					t.Errorf("empty catalog still advertises %s", claim)
				}
			}
		})
	}
}

func TestUsageAssumptionsFromInstance(t *testing.T) {
	d, err := instance.Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d.FootprintAssumptions.PUE = 1.75
	d.FootprintAssumptions.GridGPerKWh = 123
	defer instance.Select(d)()
	h, err := New(Config{Backend: newFake(), Assets: os.DirFS("../../web")})
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range []string{"en", "de"} {
		w := do(t, h, req{path: "/usage/method", header: map[string]string{"Accept-Language": lang}})
		wantStatus(t, w, 200)
		body := html.UnescapeString(w.Body.String())
		pue := "1.75"
		if lang == "de" {
			pue = "1,75"
		}
		for _, want := range []string{pue, "123", d.CopyText("pricing_method", lang), tr(lang, "PUE and electricity mix are configured assumptions, not measurements of the current provider or data location.")} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", lang, want)
			}
		}
		if strings.Contains(body, "344") {
			t.Error("stale grid assumption")
		}
	}
}

func TestHelpRequiresWiredServices(t *testing.T) {
	d, err := instance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Select(d)()
	ft := &fakeTools{d: ToolsPage{Standard: []Tool{{ID: "web-research"}, {ID: "md-to-pdf"}, {ID: "pdf"}}}}
	for _, generate := range []bool{false, true} {
		h, err := New(Config{Backend: newFake(), Assets: os.DirFS("../../web"), Tools: ft,
			Capabilities: map[string]bool{"image_generate": generate}})
		if err != nil {
			t.Fatal(err)
		}
		for _, lang := range []string{"en", "de"} {
			w := do(t, h, req{path: "/how-it-works", header: map[string]string{"Accept-Language": lang}})
			wantStatus(t, w, 200)
			body := html.UnescapeString(w.Body.String())
			for _, key := range []string{"Search and read the web", "PDF from Markdown", "Look at an image", "Feedback for [[recipient]]"} {
				if strings.Contains(body, tr(lang, key)) {
					t.Errorf("unwired service advertised: %s %s", lang, key)
				}
			}
			if strings.Contains(body, tr(lang, "Create an image")) != generate {
				t.Errorf("%s: generation availability", lang)
			}
			if strings.Contains(body, "FLUX") {
				t.Errorf("%s: full image model description without all image services", lang)
			}
			old := tr(lang, "creates an image from your description or changes an existing one, only when you ask for it. It goes to <code>[[folders.results]]</code>.")
			if strings.Contains(body, old) {
				t.Errorf("%s: editing advertised without edit model", lang)
			}
			w = do(t, h, req{path: "/help", header: map[string]string{"Accept-Language": lang}})
			body = html.UnescapeString(w.Body.String())
			if strings.Contains(body, "As PDF") || strings.Contains(body, "Als PDF") {
				t.Errorf("%s: stale base skill enabled PDF conversion help", lang)
			}
		}
	}
}

func TestOnboardingCapabilityClaims(t *testing.T) {
	for _, name := range []string{"minimal", "example"} {
		t.Run(name, func(t *testing.T) {
			d, err := instance.Load("../../../instance/" + name + "/instance.yaml")
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Select(d)()
			tools := &fakeTools{d: ToolsPage{}}
			for _, id := range []string{"web-research", "docx", "xlsx", "charts", "md-to-pdf"} {
				tools.d.Standard = append(tools.d.Standard, Tool{ID: id})
			}
			capabilities := map[string]bool{"web": true}
			h, err := New(Config{Backend: newFake(), Assets: os.DirFS("../../web"),
				Onboarding: &fakeOnboarding{seen: map[string]bool{}}, Tools: tools, Capabilities: capabilities,
				PDF: func(context.Context, []byte, string) ([]byte, error) { return nil, nil }})
			if err != nil {
				t.Fatal(err)
			}
			check := func(web, office bool) {
				t.Helper()
				for _, lang := range []string{"en", "de"} {
					w := do(t, h, req{path: "/onboarding", header: map[string]string{"Accept-Language": lang}})
					wantStatus(t, w, 200)
					body := html.UnescapeString(w.Body.String())
					for key, want := range map[string]bool{
						"It searches the web and gives you the sources.":                                                                web,
						"It reads your documents, for example Excel spreadsheets, Word files and PDFs, and analyses them.":              office,
						"It writes texts, tables, charts and PDFs and puts them into the “[[folders.results]]” folder of your project.": office,
					} {
						if strings.Contains(body, tr(lang, key)) != want {
							t.Errorf("%s %s: claim %q, want present=%v", name, lang, key, want)
						}
					}
					w = do(t, h, req{path: "/onboarding?step=9", header: map[string]string{"Accept-Language": lang}})
					wantStatus(t, w, 200)
					body = html.UnescapeString(w.Body.String())
					for id, want := range map[string]bool{"research": web, "table": office, "text": true} {
						if strings.Contains(body, `href="/?template=`+id+`"`) != want {
							t.Errorf("%s %s: template %s, want present=%v", name, lang, id, want)
						}
					}
				}
			}
			check(d.ToolEnabled("web_search"), d.ToolEnabled("md_to_pdf"))
			capabilities["web"] = false
			check(false, d.ToolEnabled("md_to_pdf"))
			capabilities["web"] = true
			tools.d.Standard = nil
			check(false, false)
		})
	}
}
