// SPDX-License-Identifier: MIT

package catalog

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle/lctest"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
)

func TestLoad(t *testing.T) {
	items, err := Load("testdata/catalog")
	if err == nil || !strings.Contains(err.Error(), "kaputt") || !strings.Contains(err.Error(), "duplicate id minutes") ||
		!strings.Contains(err.Error(), `"central-standard"`) || !strings.Contains(err.Error(), `"toolbox-without-digest"`) || !strings.Contains(err.Error(), `"foreign-kind"`) ||
		!strings.Contains(err.Error(), `"my-reserved"`) {
		t.Fatalf("want errors for kaputt and the duplicate, got %v", err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Kind+":"+it.ID+":"+it.Tier+":"+it.TitleDE)
	}
	want := []string{"mcp:web-research:standard:Im Web suchen und lesen", "mcp:md-to-pdf:optional:Ein PDF erstellen",
		"skill:minutes:optional:Protokolle schreiben", "skill:vortrag:standard:Vortrag",
		"mcp-central:workspace-knowledge:optional:Workspace-Wissen", "toolbox:media:optional:Medien bearbeiten"}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
	if items, err := Load(t.TempDir()); err != nil || len(items) != 0 {
		t.Fatalf("empty dir: %v %v", items, err)
	}
}

func TestParseFrontmatter(t *testing.T) {
	for _, c := range []struct{ in, desc, title string }{
		{"---\nname: a\ndescription: \"Mit: Doppelpunkt\"\nmetadata:\n  workspace/title: 'It''s'\n---\n", "Mit: Doppelpunkt", "It's"},
		{"---\nname: a\ndescription: >\n  zwei\n  Zeilen\nmetadata:\n  \"workspace/title\": T # Kommentar\n---\nText", "zwei Zeilen", "T"},
		{"---\r\nname: a\r\ndescription: schlicht\r\n---\r\n", "schlicht", ""},
		// a value of the wrong shape is dropped, the rest stays
		{"---\nname: a\ndescription: |\n  eins\n  zwei\nmetadata:\n  workspace/title: T\n  workspace/tags: [x, y]\n---\n", "eins\nzwei", "T"},
	} {
		fm, ok := ParseFrontmatter(c.in)
		if !ok || fm.Name != "a" || fm.Description != c.desc || fm.Meta["workspace/title"] != c.title {
			t.Errorf("%q: %+v %v", c.in, fm, ok)
		}
	}
	for _, in := range []string{"# kein Kopf", "---\nname: [a\n---\n", "---\nname: a\ndescription: " + strings.Repeat("x", maxHead) + "\n---\n"} {
		if _, ok := ParseFrontmatter(in); ok {
			t.Errorf("%.40q accepted", in)
		}
	}
}

type memStore map[string]bool

func (m memStore) OptIns(_ context.Context, uid string) ([]string, error) {
	var ids []string
	for k := range m {
		if u, id, _ := strings.Cut(k, "|"); u == uid {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (m memStore) SetOptIn(_ context.Context, uid, item string, on bool) error {
	if on {
		m[uid+"|"+item] = true
	} else {
		delete(m, uid+"|"+item)
	}
	return nil
}

var errExit3 = errors.New("exit 3")

// fixture runs the container scripts on the host against temp dirs in place
// of the container paths, and fakes the OpenCode API.
type fixture struct {
	svc           *Service
	skills, mount string
	cfg           string // ConfigFile() on the host
	reloads       atomic.Int32
	active        atomic.Bool
	running       bool
	oc            *lctest.OpenCode
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{running: true, oc: &lctest.OpenCode{Locations: []ocapi.Location{{Directory: "/"}}}}
	root := t.TempDir()
	f.skills, f.mount = filepath.Join(root, "skills"), filepath.Join(root, "catalog")
	f.cfg = filepath.Join(root, "config", "catalog.json")
	std := filepath.Join(root, "std")
	for _, d := range []string{f.mount + "/skills/minutes", f.mount + "/skills/briefe", f.mount + "/skills/vortrag", std + "/xlsx", f.skills + "/my-visitors"} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(f.mount+"/skills/minutes/SKILL.md", []byte("x"), 0o644)
	os.WriteFile(f.mount+"/skills/briefe/SKILL.md", []byte("x"), 0o644)
	os.WriteFile(std+"/xlsx/SKILL.md", []byte("---\nname: xlsx\ndescription: Excel.\n---\n"), 0o644)
	os.WriteFile(f.skills+"/my-visitors/SKILL.md", []byte("---\nname: my-visitors\ndescription: Besucherzahlen.\n---\n"), 0o644)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/active":
			if f.active.Load() {
				io.WriteString(w, `{"data":{"ses_1":{}}}`)
				return
			}
			io.WriteString(w, `{"data":{}}`)
		case "/api/location/reload":
			f.reloads.Add(1)
			f.oc.ServeHTTP(w, r)
		default:
			f.oc.ServeHTTP(w, r)
		}
	})
	f.svc = &Service{
		Dir:   "testdata/catalog",
		Store: memStore{},
		Log:   slog.New(slog.DiscardHandler),
		Client: func(string) (*ocapi.Client, bool) {
			return &ocapi.Client{BaseURL: "http://opencode", HTTP: &http.Client{Transport: handlerTransport{handler}}}, f.running
		},
		Shell: func(ctx context.Context, uid, script string, stdout io.Writer, args ...string) error {
			script = strings.NewReplacer(SkillsDir, f.skills, MountDir(), f.mount, StandardDir, std, ConfigFile(), f.cfg).Replace(script)
			cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", script, "sh"}, args...)...)
			cmd.Stdout = stdout
			err := cmd.Run()
			if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 3 {
				return errExit3
			}
			return err
		},
	}
	return f
}

func (f *fixture) links(t *testing.T) []string {
	ents, _ := os.ReadDir(f.skills)
	var out []string
	for _, e := range ents {
		if target, err := os.Readlink(filepath.Join(f.skills, e.Name())); err == nil {
			out = append(out, e.Name()+"->"+strings.TrimPrefix(target, f.mount))
		}
	}
	return out
}

func TestSetSkill(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	for _, id := range []string{"web-research", "vortrag"} {
		if _, err := f.svc.Set(ctx, "u", id, true); err != ErrUnknown {
			t.Fatalf("standard item %s switchable: %v", id, err)
		}
	}
	if _, err := f.svc.Set(ctx, "u", "gibtsnicht", true); err != ErrUnknown {
		t.Fatalf("unknown item: %v", err)
	}
	eff, err := f.svc.Set(ctx, "u", "minutes", true)
	if err != nil || eff != EffectNow || f.reloads.Load() != 1 {
		t.Fatalf("on: %v %v reloads %d", eff, err, f.reloads.Load())
	}
	if got := f.links(t); !slices.Equal(got, []string{"minutes->/skills/minutes", "vortrag->/skills/vortrag"}) {
		t.Fatalf("links %v", got)
	}
	// a stale link to a removed catalog skill and a link elsewhere: the first goes, the second stays
	os.Symlink(f.mount+"/skills/weg", f.skills+"/weg")
	os.Symlink("/somewhere", f.skills+"/fremd")
	f.active.Store(true)
	eff, err = f.svc.Set(ctx, "u", "minutes", false)
	if err != nil || eff != EffectBusy || f.reloads.Load() != 1 {
		t.Fatalf("off while busy: %v %v reloads %d", eff, err, f.reloads.Load())
	}
	if got := f.links(t); !slices.Equal(got, []string{"fremd->/somewhere", "vortrag->/skills/vortrag"}) {
		t.Fatalf("links %v", got)
	}
	if _, err := os.Stat(f.skills + "/my-visitors/SKILL.md"); err != nil {
		t.Fatal("own skill touched")
	}
	f.running = false
	if eff, err := f.svc.Set(ctx, "u", "minutes", true); err != nil || eff != EffectNextStart {
		t.Fatalf("asleep: %v %v", eff, err)
	}
	f.running = true
	f.active.Store(false)
	f.svc.AfterStart(ctx, "u")
	if got := f.links(t); !slices.Contains(got, "minutes->/skills/minutes") || f.reloads.Load() != 2 {
		t.Fatalf("after start: %v reloads %d", got, f.reloads.Load())
	}
}

type failStore struct{ memStore }

func (failStore) OptIns(context.Context, string) ([]string, error) { return nil, errors.New("db down") }

func TestGrants(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	keys := func(m map[string]bool) []string { return slices.Sorted(maps.Keys(m)) }
	allowed, off := f.svc.Grants(ctx, "u", "")
	if !slices.Equal(keys(allowed), []string{"web_read", "web_search"}) || len(off) != 1 || off["md_to_pdf"] != "Ein PDF erstellen" {
		t.Fatalf("off: %v %v", allowed, off)
	}
	if _, off := f.svc.Grants(ctx, "u", "en"); off["md_to_pdf"] != "Create a PDF" {
		t.Fatalf("off, English: %v", off)
	}
	f.svc.Set(ctx, "u", "md-to-pdf", true)
	f.svc.Set(ctx, "u", "media", true)
	if allowed, off := f.svc.Grants(ctx, "u", ""); !slices.Equal(keys(allowed), []string{"md_to_pdf", "toolbox_run", "web_read", "web_search"}) || len(off) != 0 {
		t.Fatalf("on: %v %v", allowed, off)
	}
	if allowed, off := f.svc.Grants(ctx, "andere", ""); allowed["md_to_pdf"] || allowed[ToolboxTool] || len(off) != 1 {
		t.Fatalf("other person: %v %v", allowed, off)
	}
	// fail closed: a store error leaves only the standard tools
	f.svc.Store = failStore{}
	if allowed, off := f.svc.Grants(ctx, "u", ""); !slices.Equal(keys(allowed), []string{"web_read", "web_search"}) || off["md_to_pdf"] == "" {
		t.Fatalf("store error: %v %v", allowed, off)
	}
	// a broken mcp.json grants nothing
	broken := t.TempDir()
	os.WriteFile(filepath.Join(broken, "mcp.json"), []byte(`{"items": [`), 0o644)
	f.svc.Dir, f.svc.Store = broken, memStore{}
	f.svc.Reload()
	if allowed, _ := f.svc.Grants(ctx, "u", ""); len(allowed) != 0 {
		t.Fatalf("broken catalog: %v", allowed)
	}
}

func TestBusySwitchRetried(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.active.Store(true)
	if eff, err := f.svc.Set(ctx, "u", "minutes", true); err != nil || eff != EffectBusy || f.reloads.Load() != 0 {
		t.Fatalf("busy: %v %v reloads %d", eff, err, f.reloads.Load())
	}
	f.svc.RetryPending(ctx)
	if f.reloads.Load() != 0 {
		t.Fatal("reloaded while busy")
	}
	f.active.Store(false)
	f.svc.RetryPending(ctx)
	f.svc.RetryPending(ctx)
	if f.reloads.Load() != 1 {
		t.Fatalf("idle: reloads %d, want 1", f.reloads.Load())
	}
}

func TestApplySerialized(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	shell := f.svc.Shell
	var inFlight, overlaps atomic.Int32
	f.svc.Shell = func(ctx context.Context, uid, script string, stdout io.Writer, args ...string) error {
		if inFlight.Add(1) > 1 {
			overlaps.Add(1)
		}
		defer inFlight.Add(-1)
		return shell(ctx, uid, script, stdout, args...)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := f.svc.Apply(ctx, "u"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Fatalf("%d overlapping container scripts for one person", overlaps.Load())
	}
}

// The per-person Apply locks go once the Applies are done, failed ones too
// (the fixture's shell fails for "andere").
func TestApplyLocksPruned(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() { f.svc.Apply(ctx, []string{"u", "andere"}[i%2]) })
	}
	wg.Wait()
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	if len(f.svc.applyMu) != 0 {
		t.Fatalf("%d apply locks left", len(f.svc.applyMu))
	}
}

func TestCentralConfig(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	cfg := func() string { b, _ := os.ReadFile(f.cfg); return string(b) }
	if _, err := f.svc.Set(ctx, "u", "workspace-knowledge", true); err != nil {
		t.Fatal(err)
	}
	if c := cfg(); !strings.Contains(c, `"workspace-knowledge":{`) || !strings.Contains(c, `"url":"http://llm-gw:4000/mcp/workspace-knowledge"`) {
		t.Fatalf("on: %s", c)
	}
	if got := f.svc.OnItems(ctx, "u", KindCentral); len(got) != 1 || got[0].LiteLLM[0] != "workspace_kb" {
		t.Fatalf("OnItems %+v", got)
	}
	if got := f.svc.OnItems(ctx, "andere", KindCentral); len(got) != 0 {
		t.Fatalf("other person %+v", got)
	}
	f.svc.Set(ctx, "u", "workspace-knowledge", false)
	if c := cfg(); c != `{"mcp":{"servers":{}}}` {
		t.Fatalf("off: %s", c)
	}
	if _, err := f.svc.Set(ctx, "u", "central-standard", true); err != ErrUnknown {
		t.Fatalf("invalid entry switchable: %v", err)
	}
}

func TestSkillsAndDelete(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.svc.Set(ctx, "u", "minutes", true)
	std, own, err := f.svc.Skills(ctx, "u")
	if err != nil || len(std) != 1 || std[0].ID != "xlsx" || std[0].DescriptionDE != "Excel." ||
		len(own) != 1 || own[0].ID != "my-visitors" || own[0].DescriptionDE != "Besucherzahlen." || own[0].Changed.IsZero() {
		t.Fatalf("std %+v own %+v %v", std, own, err)
	}
	for _, id := range []string{"minutes", "../catalog", "gibtsnicht", ".x"} {
		if _, err := f.svc.DeleteOwn(ctx, "u", id); err == nil {
			t.Errorf("delete %q accepted", id)
		}
	}
	if _, err := os.Stat(f.mount + "/skills/minutes/SKILL.md"); err != nil {
		t.Fatal("catalog touched")
	}
	if eff, err := f.svc.DeleteOwn(ctx, "u", "my-visitors"); err != nil || eff != EffectNow {
		t.Fatalf("delete: %v %v", eff, err)
	}
	if _, own, _ := f.svc.Skills(ctx, "u"); len(own) != 0 {
		t.Fatalf("still there: %+v", own)
	}
	f.running = false
	if _, _, err := f.svc.Skills(ctx, "u"); err != ErrNotRunning {
		t.Fatalf("asleep: %v", err)
	}
}

func TestShown(t *testing.T) {
	for in, want := range map[string]string{
		"Word lesen und schreiben. Laden bei Word, docx.":  "Word lesen und schreiben.",
		"Ohne Ausloesewoerter.":                            "Ohne Ausloesewoerter.",
		"Laden bei X.":                                     "Laden bei X.",
		"Word lesen. Laden bei Word. Load for Word, docx.": "Word lesen.",
		"Read Word. Load for Word, docx.":                  "Read Word.",
	} {
		if got := Shown(in); got != want {
			t.Errorf("Shown(%q) = %q, want %q", in, got, want)
		}
	}
}

// Exercise the HTTP contract without opening a listening socket.
type handlerTransport struct{ http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result(), nil
}
