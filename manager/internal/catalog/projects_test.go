// SPDX-License-Identifier: MIT

package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle/lctest"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func projectCatalog(t *testing.T) (*Service, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	uid := strings.Repeat("a", 64)
	if _, err := st.EnsureUser(context.Background(), identity.Identity{UID: uid, Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	s := &Service{Dir: "../../../deploy/catalog", Store: st, Projects: st, Log: slog.New(slog.DiscardHandler),
		Client: func(string) (*ocapi.Client, bool) { return nil, false },
		SessionDir: func(_ context.Context, _, session string) (string, error) {
			return "/workspace/" + strings.TrimPrefix(session, "ses_"), nil
		}}
	return s, st, uid
}

func TestProjectSelectionAndGrants(t *testing.T) {
	ctx := context.Background()
	s, st, uid := projectCatalog(t)
	for _, id := range []string{"rooms", "workspace-knowledge", "media"} {
		if err := st.SetOptIn(ctx, uid, id, true); err != nil {
			t.Fatal(err)
		}
	}
	for folder, ids := range map[string][]string{"legacy": nil, "wiki": {"workspace-knowledge"}, "empty": {}} {
		if err := st.CreateProject(ctx, store.Project{ID: folder, UID: uid, Folder: folder, CreatedAt: time.Now(), Capabilities: ids}); err != nil {
			t.Fatal(err)
		}
	}
	defaults, err := s.Selection(ctx, uid, nil)
	if err != nil || !slices.Equal(defaults, []string{"workspace-knowledge", "web-research"}) {
		t.Fatal(defaults, err)
	}
	if _, err := s.Selection(ctx, uid, []string{"library"}); err != ErrUnknown {
		t.Fatal("unavailable selection", err)
	}
	if ids, err := s.Selection(ctx, uid, []string{}); err != nil || ids == nil || len(ids) != 0 {
		t.Fatal("explicit none", ids, err)
	}
	for _, tc := range []struct {
		project, session, uid string
		rooms, web            bool
	}{
		{"legacy", "", uid, true, true}, {"wiki", "", uid, false, false}, {"empty", "", uid, false, false},
		{"", "ses_legacy", uid, true, true}, {"wiki", "ses_legacy", uid, false, false},
		{"legacy", "", strings.Repeat("b", 64), false, false}, {"missing", "", uid, false, false}, {"", "", uid, false, false},
	} {
		c := WithProject(logx.With(ctx, logx.Fields{SessionID: tc.session}), tc.project)
		allowed, _ := s.Grants(c, tc.uid, "en")
		if allowed["rooms_free"] != tc.rooms || allowed["web_search"] != tc.web {
			t.Fatalf("%+v: %v", tc, allowed)
		}
	}
	if items := s.OnItems(WithProject(ctx, "wiki"), uid, KindCentral); len(items) != 1 || items[0].ID != "workspace-knowledge" {
		t.Fatal(items)
	}
	if items := s.OnItems(WithProject(ctx, "wiki"), uid, KindToolbox); len(items) != 0 {
		t.Fatal(items)
	}
	if _, err := s.SetProject(ctx, uid, "wiki", []string{"workspace-knowledge", "rooms"}); err != nil {
		t.Fatal(err)
	}
	if allowed, _ := s.Grants(WithProject(ctx, "wiki"), uid, "de"); !allowed["rooms_free"] {
		t.Fatal(allowed)
	}
	if err := st.SetOptIn(ctx, uid, "rooms", false); err != nil {
		t.Fatal(err)
	}
	if allowed, _ := s.Grants(WithProject(ctx, "wiki"), uid, "de"); allowed["rooms_free"] {
		t.Fatal("global removal ignored")
	}
	d, _ := testinstance.Load("../../../instance/example/instance.yaml")
	d.Capabilities.Projects = false
	restore := instance.Select(d)
	defer restore()
	if allowed, _ := s.Grants(ctx, uid, "de"); !allowed["web_search"] {
		t.Fatal("off switch")
	}
	d.Capabilities.Projects = true
	d.Capabilities.LegacyDefault = "none"
	if allowed, _ := s.Grants(WithProject(ctx, "legacy"), uid, "de"); len(allowed) != 0 {
		t.Fatal("migration none", allowed)
	}
}

func TestProjectConfigPreservesSettingsAndOwnSkills(t *testing.T) {
	root, catalog := t.TempDir(), t.TempDir()
	skills := filepath.Join(root, ".opencode", "skills")
	for _, dir := range []string{filepath.Join(skills, "own"), filepath.Join(catalog, "minutes")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	f := filepath.Join(root, "opencode.json")
	if err := os.WriteFile(f, []byte(`{"model":"custom","mcp":{"servers":{"own":{"type":"local","command":["test"]}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(ids ...string) {
		t.Helper()
		args := append([]string{"-c", projectConfigScript, "sh", root, `{"workspace":{"url":"http://llm-gw:4000/mcp/project/wiki/workspace"}}`, catalog}, ids...)
		if out, err := exec.Command("sh", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run("minutes")
	if _, err := os.Readlink(filepath.Join(skills, "minutes")); err != nil {
		t.Fatal(err)
	}
	run()
	if _, err := os.Lstat(filepath.Join(skills, "minutes")); !os.IsNotExist(err) {
		t.Fatal("catalog skill not removed", err)
	}
	if _, err := os.Stat(filepath.Join(skills, "own")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil || data["model"] != "custom" || !strings.Contains(string(b), `"own"`) {
		t.Fatal(string(b), err)
	}
	if err := ValidateDefaults((&Service{Dir: "../../../deploy/catalog", Log: slog.New(slog.DiscardHandler)}).Items()); err != nil {
		t.Fatal(err)
	}
}

func TestProjectToggleDefersReloadAndRendersScope(t *testing.T) {
	ctx := context.Background()
	_, st, uid := projectCatalog(t)
	f := newFixture(t)
	f.svc.Projects, f.svc.Store = st, st
	f.svc.Dir = "../../../deploy/catalog"
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "wiki"), 0755); err != nil {
		t.Fatal(err)
	}
	shell := f.svc.Shell
	f.svc.Shell = func(ctx context.Context, uid, script string, out io.Writer, args ...string) error {
		for i := range args {
			args[i] = strings.ReplaceAll(args[i], "/workspace/", root+"/")
			args[i] = strings.ReplaceAll(args[i], MountDir(), f.mount)
		}
		return shell(ctx, uid, script, out, args...)
	}
	if err := st.CreateProject(ctx, store.Project{ID: "wiki", UID: uid, Folder: "wiki", CreatedAt: time.Now(), Capabilities: []string{"workspace-knowledge"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"rooms", "workspace-knowledge"} {
		if err := st.SetOptIn(ctx, uid, id, true); err != nil {
			t.Fatal(err)
		}
	}
	f.active.Store(true)
	effect, err := f.svc.SetProject(ctx, uid, "wiki", []string{"workspace-knowledge", "rooms"})
	if err != nil || effect != EffectBusy || f.reloads.Load() != 0 || !f.svc.pending[uid] {
		t.Fatal(effect, err, f.svc.pending)
	}
	b, err := os.ReadFile(filepath.Join(root, "wiki", "opencode.json"))
	if err != nil || !strings.Contains(string(b), "/mcp/project/wiki/workspace") || !strings.Contains(string(b), "/mcp/project/wiki/workspace-knowledge") {
		t.Fatal(string(b), err)
	}
	f.active.Store(false)
	f.svc.RetryPending(ctx)
	if f.reloads.Load() != 1 || len(f.svc.pending) != 0 {
		t.Fatal("reload not retried", f.reloads.Load(), f.svc.pending)
	}
	if _, err := f.svc.SetProject(ctx, uid, "wiki", []string{}); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(filepath.Join(root, "wiki", "opencode.json"))
	var config struct {
		MCP struct {
			Servers map[string]struct{ Disabled bool }
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &config); err != nil || !config.MCP.Servers["workspace-knowledge"].Disabled {
		t.Fatal(string(b), err)
	}
}

func TestCapabilityDefaultsAndOffSwitch(t *testing.T) {
	ctx := context.Background()
	s, st, uid := projectCatalog(t)
	d, err := testinstance.Load("../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	restore := instance.Select(d)
	defer restore()
	if err := st.SetOptIn(ctx, uid, "rooms", true); err != nil {
		t.Fatal(err)
	}
	d.Capabilities.ProjectDefault = instance.CapabilityDefault{Mode: "none"}
	if ids, err := s.Selection(ctx, uid, nil); err != nil || ids == nil || len(ids) != 0 {
		t.Fatal(ids, err)
	}
	d.Capabilities.ProjectDefault.Mode = "all"
	ids, err := s.Selection(ctx, uid, nil)
	if err != nil || !slices.Contains(ids, "rooms") || !slices.Contains(ids, "web-research") || slices.Contains(ids, "workspace-knowledge") {
		t.Fatal(ids, err)
	}
	d.Capabilities.ProjectDefault = instance.CapabilityDefault{IDs: []string{"missing"}}
	if err := ValidateDefaults(s.Items()); err == nil {
		t.Fatal("unknown default accepted")
	}
	// A fixed assistant must never be converted into a mutable normal project.
	p := store.Project{ID: "fixed", UID: uid, Folder: "fixed", CreatedAt: time.Now()}
	def := instance.Current().Assistants[0]
	p.Assistant = instance.Current().AssistantPin(def)
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetProject(ctx, uid, p.Folder, []string{"rooms"}); err != ErrUnknown {
		t.Fatal("assistant toggled", err)
	}
	d.Capabilities.Projects = false
	if _, err := s.SetProject(ctx, uid, "fixed", []string{"rooms"}); err != ErrUnknown {
		t.Fatal("off switch ignored", err)
	}
}

func TestApplyProjectsContinuesAfterBrokenProject(t *testing.T) {
	for _, broken := range []string{"missing", "invalid-json", "shell-error"} {
		t.Run(broken, func(t *testing.T) {
			ctx := context.Background()
			s, st, uid := projectCatalog(t)
			rt := lctest.NewRuntime()
			rt.Root = t.TempDir()
			var stderr bytes.Buffer
			failure := errors.New("exec failed")
			var visited []string
			s.Shell = func(ctx context.Context, uid, script string, out io.Writer, args ...string) error {
				visited = append(visited, filepath.Base(args[0]))
				if broken == "shell-error" && filepath.Base(args[0]) == "middle" {
					return failure
				}
				code, err := rt.Exec(ctx, "oc-user1", append([]string{"sh", "-c", script, "sh"}, args...), nil, out, &stderr)
				if err == nil && code != 0 {
					err = fmt.Errorf("exit %d: %s", code, stderr.String())
				}
				return err
			}
			for i, folder := range []string{"first", "middle", "last"} {
				if err := st.CreateProject(ctx, store.Project{ID: folder, UID: uid, Folder: folder, CreatedAt: time.Now().Add(-time.Duration(i) * time.Hour)}); err != nil {
					t.Fatal(err)
				}
				if folder == "middle" && broken == "missing" {
					continue
				}
				dir := filepath.Join(rt.Root, folder)
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
				if folder == "middle" && broken == "invalid-json" {
					if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte("{broken"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := s.applyProjects(ctx, uid)
			if broken == "shell-error" {
				if !errors.Is(err, failure) {
					t.Fatal("missing project error", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(visited, []string{"first", "middle", "last"}) {
				t.Fatal("projects skipped", visited)
			}
			for _, folder := range visited {
				if folder == "middle" && broken != "invalid-json" {
					continue
				}
				b, err := os.ReadFile(filepath.Join(rt.Root, folder, "opencode.json"))
				if err != nil || !json.Valid(b) || !strings.Contains(string(b), "/mcp/project/"+folder+"/") {
					t.Fatalf("%s config: %s, %v", folder, b, err)
				}
			}
			if broken == "invalid-json" && !strings.Contains(stderr.String(), "invalid JSON") {
				t.Fatal("missing JSON diagnostic", stderr.String())
			}
		})
	}
}
