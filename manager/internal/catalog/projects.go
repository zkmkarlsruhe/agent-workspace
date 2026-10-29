// SPDX-License-Identifier: MIT

package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type ProjectStore interface {
	Projects(context.Context, string) ([]store.Project, error)
	ProjectByID(context.Context, string, string) (store.Project, error)
	ProjectByFolder(context.Context, string, string) (store.Project, error)
	SetProjectCapabilities(context.Context, string, string, []string) error
}

type projectKey struct{}

// WithProject binds discovery to a project. Calls also validate the session's directory.
func WithProject(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, projectKey{}, id)
}

func (s *Service) ProjectEnabled() bool {
	return s.Enabled() && s.Projects != nil && instance.Current().Capabilities.Projects
}

// Available includes standard catalog entries and the person's global opt-ins.
func (s *Service) Available(ctx context.Context, uid string) ([]Item, error) {
	on, err := s.On(ctx, uid)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(s.Items(), func(it Item) bool { return it.Tier != TierStandard && !on[it.ID] }), nil
}

// Selection validates an explicit selection; nil means the instance creation default.
func (s *Service) Selection(ctx context.Context, uid string, ids []string) ([]string, error) {
	available, err := s.Available(ctx, uid)
	if err != nil {
		return nil, err
	}
	valid := map[string]bool{}
	for _, it := range available {
		valid[it.ID] = true
	}
	if ids != nil {
		for _, id := range ids {
			if !valid[id] {
				return nil, ErrUnknown
			}
		}
		selected := slices.Clone(ids)
		slices.Sort(selected)
		return slices.Compact(selected), nil
	}
	out := []string{}
	def := instance.Current().Capabilities.ProjectDefault
	for _, it := range available {
		if def.Mode == "all" || slices.Contains(def.IDs, it.ID) {
			out = append(out, it.ID)
		}
	}
	return out, nil
}

func (s *Service) ProjectItems(ctx context.Context, uid string, p store.Project) ([]Item, error) {
	if p.Assistant.ID != "" {
		return nil, ErrUnknown
	}
	items, err := s.Available(ctx, uid)
	if err != nil {
		return nil, err
	}
	if !s.ProjectEnabled() || (p.Capabilities == nil && instance.Current().Capabilities.LegacyDefault == "global") {
		return items, nil
	}
	return slices.DeleteFunc(items, func(it Item) bool { return !slices.Contains(p.Capabilities, it.ID) }), nil
}

// scopedItems fails closed for missing, foreign, deleted or mismatched projects/sessions.
func (s *Service) scopedItems(ctx context.Context, uid string) ([]Item, error) {
	if !s.ProjectEnabled() {
		return s.Available(ctx, uid)
	}
	id, _ := ctx.Value(projectKey{}).(string)
	session := logx.FromContext(ctx).SessionID
	var p store.Project
	var err error
	if session != "" {
		if s.SessionDir == nil {
			return nil, ErrUnknown
		}
		dir, e := s.SessionDir(ctx, uid, session)
		if e != nil {
			return nil, e
		}
		folder, ok := strings.CutPrefix(dir, "/workspace/")
		if !ok || folder == "" || path.Base(folder) != folder {
			return nil, ErrUnknown
		}
		p, err = s.Projects.ProjectByFolder(ctx, uid, folder)
		if id != "" && p.ID != id {
			return nil, ErrUnknown
		}
	} else if id != "" {
		p, err = s.Projects.ProjectByID(ctx, uid, id)
	} else {
		return nil, ErrUnknown
	}
	if err != nil {
		return nil, err
	}
	return s.ProjectItems(ctx, uid, p)
}

func (s *Service) SetProject(ctx context.Context, uid, folder string, ids []string) (Effect, error) {
	if !s.ProjectEnabled() {
		return 0, ErrUnknown
	}
	p, err := s.Projects.ProjectByFolder(ctx, uid, folder)
	if err != nil {
		return 0, err
	}
	if p.Assistant.ID != "" {
		return 0, ErrUnknown
	}
	if ids == nil {
		ids = []string{}
	}
	ids, err = s.Selection(ctx, uid, ids)
	if err != nil {
		return 0, err
	}
	if err := s.Projects.SetProjectCapabilities(ctx, uid, folder, ids); err != nil {
		return 0, err
	}
	return s.applyOrDefer(WithProject(ctx, p.ID), uid)
}

// ValidateDefaults catches misspelled catalog IDs at startup, including disabled entries.
func ValidateDefaults(items []Item) error {
	for _, id := range instance.Current().Capabilities.ProjectDefault.IDs {
		if !slices.ContainsFunc(items, func(it Item) bool { return it.ID == id }) {
			return fmt.Errorf("capabilities.project_default: unknown catalog ID %q", id)
		}
	}
	return nil
}

// Prepare runs before session creation, so a newly created project gets its own discovery URL.
func (s *Service) Prepare(ctx context.Context, uid, projectID string) error {
	_, err := s.Apply(WithProject(ctx, projectID), uid)
	return err
}

func (s *Service) applyProjects(ctx context.Context, uid string) error {
	if s.Projects == nil {
		return nil
	}
	var projects []store.Project
	var err error
	if id, _ := ctx.Value(projectKey{}).(string); id != "" {
		var p store.Project
		p, err = s.Projects.ProjectByID(ctx, uid, id)
		projects = []store.Project{p}
	} else {
		projects, err = s.Projects.Projects(ctx, uid)
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range projects {
		if p.Assistant.ID != "" {
			continue
		}
		if err := s.applyProject(ctx, uid, p); err != nil {
			s.Log.WarnContext(ctx, "project capabilities failed", "uid", uid, "project", p.ID, "err", err)
			errs = append(errs, fmt.Errorf("project %s: %w", p.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) applyProject(ctx context.Context, uid string, p store.Project) error {
	items, err := s.ProjectItems(ctx, uid, p)
	if err != nil {
		return err
	}
	on := map[string]bool{}
	var skills []string
	for _, it := range items {
		on[it.ID] = true
		if s.ProjectEnabled() && it.Kind == KindSkill {
			skills = append(skills, it.ID)
		}
	}
	servers := map[string]any{}
	base := centralURL
	if s.ProjectEnabled() {
		base += "project/" + p.ID + "/"
	}
	remote := func(id string, enabled bool) any {
		return map[string]any{"type": "remote", "url": base + id, "oauth": false, "codemode": false, "disabled": !enabled,
			"timeout": map[string]int{"startup": 10000, "catalog": 15000, "execution": 60000}}
	}
	if instance.Current().HasBuiltinTools() {
		servers[instance.Current().MCPServer] = remote(instance.Current().MCPServer, true)
	}
	for _, it := range s.Items() {
		if it.Kind == KindCentral {
			servers[it.ID] = remote(it.ID, on[it.ID])
		}
	}
	b, err := json.Marshal(servers)
	if err != nil {
		return err
	}
	// Project names are store-owned, but validate before passing filesystem paths to the shell.
	if path.Base(p.Folder) != p.Folder || strings.HasPrefix(p.Folder, ".") {
		return errors.New("invalid project folder")
	}
	args := []string{"/workspace/" + p.Folder, string(b), MountDir() + "/skills"}
	args = append(args, skills...)
	if err := s.Shell(ctx, uid, projectConfigScript, nil, args...); err != nil {
		return fmt.Errorf("project capabilities: %w", err)
	}
	return nil
}

// Preserve other project settings and real skill folders; only catalog links are managed.
const projectConfigScript = `python3 - "$@" <<'PY'
import json, os, pathlib, sys
root = pathlib.Path(sys.argv[1])
if not root.is_dir():
    raise SystemExit(0)
f = root / 'opencode.json'
try:
    data = json.loads(f.read_text()) if f.exists() else {}
except json.JSONDecodeError as err:
    print(f"{f}: invalid JSON: {err}", file=sys.stderr)
    data = {}
data.setdefault('mcp', {}).setdefault('servers', {}).update(json.loads(sys.argv[2]))
tmp = f.with_name('.opencode-capabilities.tmp')
with tmp.open('w') as out:
    json.dump(data, out)
os.replace(tmp, f)
skills = root / '.opencode' / 'skills'
skills.mkdir(parents=True, exist_ok=True)
catalog = pathlib.Path(sys.argv[3])
wanted = set(sys.argv[4:])
for link in skills.iterdir():
    if link.is_symlink() and str(link.readlink()).startswith(str(catalog) + '/'):
        if link.name not in wanted or link.readlink() != catalog / link.name or not link.exists():
            link.unlink()
for name in wanted:
    link = skills / name
    if not link.exists() and not link.is_symlink() and (catalog / name).is_dir():
        link.symlink_to(catalog / name)
PY
`

func (s *Service) ProjectAccess(ctx context.Context, uid string) bool {
	if !s.ProjectEnabled() {
		return true
	}
	_, err := s.scopedItems(ctx, uid)
	return err == nil
}
