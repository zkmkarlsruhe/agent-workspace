// SPDX-License-Identifier: MIT

package instance

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

var builtin, _ = embedded.ReadFile("example.yaml")

func TestEmbeddedDefaultSelection(t *testing.T) {
	d, err := Load("")
	if Current().Default {
		if err != nil || !d.Default || !reflect.DeepEqual(d.Values, Current().Values) {
			t.Fatalf("flagged default differs from initial selection: %v", err)
		}
	} else {
		if err == nil || !strings.Contains(err.Error(), "OC_INSTANCE_FILE") {
			t.Fatalf("unflagged comprehensive instance selected: %v", err)
		}
		if Current().Values["brand_id"] != "workspace" || len(Current().resolved) != 0 {
			t.Fatal("unselected initial values are not neutral")
		}
	}
}

func TestInstanceDefaultIsNeutral(t *testing.T) {
	minimal, err := embedded.ReadFile("minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	unflagged := bytes.ReplaceAll(builtin, []byte("default: true\n"), nil)
	flagged := append([]byte("default: true\n"), unflagged...)
	for name, files := range map[string]fstest.MapFS{
		"none":               {},
		"comprehensive only": {"other.yaml": {Data: unflagged}},
		"multiple unflagged": {"minimal.yaml": {Data: minimal}, "other.yaml": {Data: unflagged}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadDefault(files); err == nil || !strings.Contains(err.Error(), "OC_INSTANCE_FILE") {
				t.Fatalf("accepted non-neutral or ambiguous default: %v", err)
			}
		})
	}
	for _, files := range []fstest.MapFS{
		{"other.yaml": {Data: flagged}},
		{"minimal.yaml": {Data: minimal}, "other.yaml": {Data: flagged}},
	} {
		d, err := loadDefault(files)
		if err != nil || !d.Default || len(d.resolved) == 0 {
			t.Fatalf("flagged default: %v, %v", d, err)
		}
	}
	if _, err := loadDefault(fstest.MapFS{"a.yaml": {Data: flagged}, "b.yaml": {Data: flagged}}); err == nil || !strings.Contains(err.Error(), "multiple embedded definitions") {
		t.Fatalf("duplicate defaults accepted: %v", err)
	}
	d, err := loadDefault(fstest.MapFS{"minimal.yaml": {Data: minimal}})
	if err != nil || d.Default || d.Values["brand_id"] != "workspace" || len(d.resolved) != 0 {
		t.Fatalf("sole minimal: %v, %v", d, err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing explicit file fell back")
	}
}

func TestCommittedInstances(t *testing.T) {
	for _, name := range []string{"minimal", "example"} {
		dir := filepath.Join("../../../instance", name)
		b, err := os.ReadFile(filepath.Join(dir, "instance.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := embedded.ReadFile(name + ".yaml")
		if err != nil || !bytes.Equal(b, snapshot) {
			t.Fatalf("%s embedded snapshot differs: %v", name, err)
		}
		d, err := Parse(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wantArchiveDays := 0.0
		if name == "example" {
			wantArchiveDays = 30
		}
		if d.Sessions.ArchiveAfterDays != wantArchiveDays {
			t.Fatalf("%s: archive_after_days=%g, want %g", name, d.Sessions.ArchiveAfterDays, wantArchiveDays)
		}
		out := filepath.Join(dir, "rendered")
		if name == "example" {
			if !bytes.Equal(b, builtin) {
				t.Fatal("embedded Workspace snapshot differs from instance/example/instance.yaml")
			}
			out = "../../../instance/example/rendered"
		}
		for file, want := range d.Render() {
			got, err := os.ReadFile(filepath.Join(out, file))
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s/%s differs from renderer: %v", name, file, err)
			}
			if name == "minimal" {
				for _, forbidden := range []string{"privatebrand", "privateteam", "privateperson"} {
					if strings.Contains(strings.ToLower(string(want)), forbidden) {
						t.Errorf("minimal %s contains %s", file, forbidden)
					}
				}
			}
		}
	}
}

func TestValidation(t *testing.T) {
	for name, change := range map[string]func(*Definition){
		"shared collision": func(d *Definition) {
			d.Assistants[1].ProjectMode = "shared"
			d.Assistants[1].Title = d.Assistants[0].Title
		},
		"shared reserved":      func(d *Definition) { d.Assistants[0].Title = d.Folders.Results },
		"MCP namespace":        func(d *Definition) { d.Assistants[0].MCPTools = []string{"other.web_search"} },
		"schema":               func(d *Definition) { d.Schema = 2 },
		"folder traversal":     func(d *Definition) { d.Folders.Inbox = "../inbox" },
		"folder collision":     func(d *Definition) { d.Folders.Work = d.Folders.Results },
		"hidden results":       func(d *Definition) { d.Folders.Results = ".results" },
		"visible trash":        func(d *Definition) { d.Folders.Trash = "trash" },
		"marker injection":     func(d *Definition) { d.Values["profile_marker"] = "x -->" },
		"filename traversal":   func(d *Definition) { d.Values["config_file"] = "../config.json" },
		"shell injection":      func(d *Definition) { d.Values["catalog_mount"] = "/opt/$(id)" },
		"unsafe URL":           func(d *Definition) { d.Values["support_url"] = "javascript:alert(1)" },
		"secret":               func(d *Definition) { d.Defaults["LITELLM_API_KEY"] = "secret" },
		"nextcloud traversal":  func(d *Definition) { d.Values["nextcloud_root"] = "../shared" },
		"endpoint credentials": func(d *Definition) { d.Defaults["LITELLM_BASE_URL"] = "https://user:secret@example.org" },
		"model":                func(d *Definition) { d.Models.Small = "" },
		"consent":              func(d *Definition) { d.Consent.EN.Points = nil },
		"unknown token":        func(d *Definition) { d.Artifacts["AGENTS.md"] = "[[unknown]]" },
		"artifact traversal":   func(d *Definition) { d.Artifacts["../outside"] = "x" },
	} {
		t.Run(name, func(t *testing.T) {
			d, err := Load("../../../instance/example/instance.yaml")
			if err != nil {
				t.Fatal(err)
			}
			change(d)
			if err := d.Validate(); err == nil {
				t.Fatal("invalid instance accepted")
			}
		})
	}
	for _, suffix := range []string{"\nunknown: true\n", "\n---\nschema: 1\n", "\nschema: 1\n"} {
		if _, err := Parse(append(append([]byte(nil), builtin...), []byte(suffix)...)); err == nil {
			t.Fatalf("accepted unknown field, extra document or duplicate key: %q", suffix)
		}
	}
}

func TestTextEscaping(t *testing.T) {
	d, err := Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d.Values["name"] = `<script>"50% & more"</script>`
	if got := d.Text("<b>[[name]]</b> %s", true, true); got != "<b>&lt;script&gt;&#34;50%% &amp; more&#34;&lt;/script&gt;</b> %s" {
		t.Fatalf("unsafe HTML or format string: %s", got)
	}
	if got := d.Text("User wrote Workspace and Alice", false, false); got != "User wrote Workspace and Alice" {
		t.Fatal("renderer must only expand explicit tokens")
	}
}

func TestEnglishFolderDefaults(t *testing.T) {
	start := bytes.Index(builtin, []byte("folders:\n"))
	end := bytes.Index(builtin, []byte("models:\n"))
	if start < 0 || end <= start {
		t.Fatal("fixture has no folder section")
	}
	b := append(append([]byte(nil), builtin[:start]...), builtin[end:]...)
	d, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Folders != (Folders{Inbox: "inbox", Results: "results", Work: "work", ProjectMeta: ".project", Brief: "brief.md", Trash: ".trash"}) {
		t.Fatalf("non-English defaults: %+v", d.Folders)
	}
}

func TestAssistantsAreOptional(t *testing.T) {
	i := bytes.Index(builtin, []byte("\nassistants:\n"))
	if i < 0 {
		t.Fatal("no profile example")
	}
	legacy, err := Parse(builtin[:i])
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy.Assistants) != 0 {
		t.Fatal("legacy instance has profiles")
	}
	current, err := Parse(builtin)
	if err != nil {
		t.Fatal(err)
	}
	// Untuned profiles leave existing assets unchanged; tuned variants are tested separately.
	for n := range current.Assistants {
		current.Assistants[n].Temperature, current.Assistants[n].TopP = nil, nil
		current.Assistants[n].MaxOutputTokens, current.Assistants[n].ReasoningEffort = nil, ""
	}
	for name, want := range legacy.Render() {
		if !bytes.Equal(want, current.Render()[name]) {
			t.Fatalf("profile changed legacy asset %s", name)
		}
	}
	current.Assistants = append(current.Assistants, current.Assistants[0])
	if current.Validate() == nil {
		t.Fatal("duplicate profile accepted")
	}
	if _, err := Parse(append(append([]byte(nil), builtin...), []byte("    unknown_profile_setting: true\n")...)); err == nil {
		t.Fatal("unknown profile field accepted")
	}
}

func TestKnowledgeReferences(t *testing.T) {
	d, err := Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.KnowledgeSources) != 4 {
		t.Fatal("expected four existing MCP sources")
	}
	p := d.Assistants[0]
	before := d.AssistantPin(p)
	source := d.KnowledgeSources["workspace_kb_wiki"]
	source.Tools = []string{"workspace_wiki"}
	d.KnowledgeSources["workspace_kb_wiki"] = source
	if d.AssistantPin(p) == before {
		t.Fatal("source policy did not change profile hash")
	}
	delete(d.KnowledgeSources, "workspace_kb_wiki")
	if d.Validate() == nil {
		t.Fatal("dangling profile source accepted")
	}
}

func TestLegacyAssistantsConfig(t *testing.T) {
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	current, err := Parse(builtin)
	if err != nil {
		t.Fatal(err)
	}
	legacyYAML := bytes.ReplaceAll(builtin, []byte("\nassistants:\n"), []byte("\nprofiles:\n"))
	legacyYAML = bytes.ReplaceAll(legacyYAML, []byte("[[assistants."), []byte("[[profiles."))
	legacy, err := Parse(legacyYAML)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "profiles is deprecated; use assistants") {
		t.Fatal("missing deprecation warning", log.String())
	}
	if !reflect.DeepEqual(current.Assistants, legacy.Assistants) || !reflect.DeepEqual(current.Render(), legacy.Render()) {
		t.Fatal("legacy configuration changes definitions or runtime assets")
	}
	for _, assistant := range current.Assistants {
		old, ok := legacy.Assistant(assistant.ID)
		if !ok || current.AssistantPin(assistant) != legacy.AssistantPin(old) {
			t.Fatal("legacy configuration changes stored pins")
		}
	}
	both, err := Parse(append(legacyYAML, []byte("\nassistants: []\n")...))
	if err != nil || len(both.Assistants) != 0 {
		t.Fatalf("explicit empty assistants must override legacy definitions: %v", err)
	}
	invalid := bytes.Replace(legacyYAML, []byte("    model_role:"), []byte("    unknown_field:"), 1)
	if _, err := Parse(invalid); err == nil {
		t.Fatal("legacy definitions bypass strict validation")
	}
}

func TestDemoAssistantOptions(t *testing.T) {
	d, err := Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	help, _ := d.Assistant("it-hilfe")
	research, ok := d.Assistant("research")
	if !ok || help.ProjectMode != "shared" || research.ProjectMode != "per_task" || research.Title != "Research" || research.EntryQuestion != "Was möchtest du recherchieren?" || *research.Steps != 40 {
		t.Fatal(help, research)
	}
	sources, err := research.Sources(d.KnowledgeSources)
	if err != nil || len(sources) != 1 || sources[0].MCPServer != "library/opac" {
		t.Fatal(sources, err)
	}
	agent := string(d.Render()["agents/profile-research.md"])
	for _, want := range []string{"workspace_web_search", "workspace_web_read", "library_catalog_search", "library_catalog_item", "results/"} {
		if !strings.Contains(agent, want) {
			t.Fatal("missing research grant/instruction", want)
		}
	}
	d.Assistants[1].Enabled = false
	if d.Assistants[1].Available(nil) || !strings.Contains(string(d.Render()["agents/profile-research.md"]), "disabled: true") {
		t.Fatal("research off switch ignored")
	}
}
