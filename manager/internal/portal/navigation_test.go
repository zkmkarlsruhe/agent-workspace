// SPDX-License-Identifier: MIT

package portal

import (
	"html"
	"os"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistantchat"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func TestNavigation(t *testing.T) {
	h, err := New(Config{Backend: newFake(), TaskAssistants: &assistantchat.Service{},
		Settings: &fakeSettings{}, Tools: &fakeTools{}, Connections: &fakeConns{},
		Assets: os.DirFS("../../web"), Origin: "https://apps.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"de", "en"} {
		w := do(t, h, req{path: "/tools", header: map[string]string{"Accept-Language": language}})
		wantStatus(t, w, 200)
		body := html.UnescapeString(w.Body.String())
		_, main, ok := strings.Cut(body, `<nav class="main-nav"`)
		if !ok {
			t.Fatal("missing main navigation")
		}
		main, _, _ = strings.Cut(main, "</nav>")
		if strings.Count(main, "<li>") != 4 {
			t.Fatal("main navigation must have four entries", main)
		}
		wantOrder(t, main, []string{`href="/"`, `href="/assistants"`, `href="/tools" aria-current="page"`, `href="/help"`})
		_, account, ok := strings.Cut(body, `<details class="menu disclosure account-menu">`)
		if !ok {
			t.Fatal("missing native account disclosure")
		}
		account, _, _ = strings.Cut(account, "</details>")
		wantOrder(t, account, []string{"<summary", "</summary>", `href="/settings"`, `href="/usage"`, `href="/connections"`, `href="/privacy"`, `href="/environment/delete"`})
		if strings.Contains(account, "onclick") || strings.Contains(account, `role="menu"`) {
			t.Fatal("account links must work with native disclosure semantics")
		}
		labels := []string{"My projects", "Assistants", "Capabilities", "Help", "About me & how I work", "Connections (Nextcloud)", "Delete account", "Choose extra abilities for your tasks, such as searching documents or working with tables."}
		if language == "de" {
			labels = []string{"Meine Projekte", "Assistenten", "Fähigkeiten", "Hilfe", "Über mich & Arbeitsweise", "Verbindungen (Nextcloud)", "Konto löschen", "Wähle zusätzliche Fähigkeiten für deine Aufgaben, etwa Dokumente durchsuchen oder Tabellen bearbeiten."}
		}
		for _, label := range labels {
			if !strings.Contains(body, label) {
				t.Errorf("%s: missing %q", language, label)
			}
		}
	}
}

func wantOrder(t *testing.T, text string, items []string) {
	t.Helper()
	for _, item := range items {
		_, remaining, ok := strings.Cut(text, item)
		if !ok {
			t.Fatalf("missing or out of order %q in %s", item, text)
		}
		text = remaining
	}
}

func TestMinimalCapabilitiesCopy(t *testing.T) {
	definition, err := instance.Load("../../../instance/minimal/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Select(definition)()
	h, _ := setupTools(t)
	for _, language := range []string{"de", "en"} {
		w := do(t, h, req{path: "/tools", header: map[string]string{"Accept-Language": language}})
		wantStatus(t, w, 200)
		body := html.UnescapeString(w.Body.String())
		if strings.Contains(strings.ToLower(body), "\x7a\x6b\x6d") {
			t.Fatalf("%s: minimal capabilities page contains Workspace branding", language)
		}
		want := "Choose extra abilities for your tasks, such as searching documents or working with tables."
		if language == "de" {
			want = "Wähle zusätzliche Fähigkeiten für deine Aufgaben, etwa Dokumente durchsuchen oder Tabellen bearbeiten."
		}
		if !strings.Contains(body, want) {
			t.Fatalf("%s: missing instance-specific capabilities subtitle %q", language, want)
		}
	}
}
