// SPDX-License-Identifier: MIT

package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
)

// Container paths (docs/adr/README.md §3.12): real folders in SkillsDir are own
// skills, symlinks into MountDir/skills are switched-on catalog skills.
const (
	SkillsDir   = "/home/user/.config/opencode/skills"
	StandardDir = "/etc/opencode/skills"
	// ConfigFile is the person's OPENCODE_CONFIG (lifecycle spec): the opted-in
	// mcp-central servers. Measured (2.0.30): a missing file is ignored, and a
	// written or emptied file takes effect with location/reload.
	// centralURL + id is the server on the llm-gw listener (cmd/manager, gateway/central).
	centralURL = "http://llm-gw:4000/mcp/"
)

var (
	ErrUnknown    = errors.New("catalog: unknown or not switchable item")
	ErrNotRunning = errors.New("catalog: environment not running")
	ErrNotFound   = errors.New("catalog: own skill not found")
)

// Store keeps each person's switched-on catalog ids.
type Store interface {
	OptIns(ctx context.Context, uid string) ([]string, error)
	SetOptIn(ctx context.Context, uid, item string, on bool) error
}

type Service struct {
	Dir        string // OC_CATALOG_DIR as the manager sees it; "" = feature off
	Store      Store
	Projects   ProjectStore
	SessionDir func(context.Context, string, string) (string, error)
	// Shell runs script with args as uid 1000 in the person's running
	// container (lifecycle.Service.Shell, D33); exit 3 = not found.
	Shell func(ctx context.Context, uid, script string, stdout io.Writer, args ...string) error
	// Client returns the OpenCode client of a running environment.
	Client func(uid string) (*ocapi.Client, bool)
	// Running lists the uids of running environments (lifecycle.Service.RunningUIDs).
	Running func() []string
	Log     *slog.Logger

	cacheMu sync.Mutex
	loaded  bool
	items   []Item

	mu      sync.Mutex
	pending map[string]bool       // owed an Apply once the helper is idle (Set, ReloadAll)
	applyMu map[string]*applyLock // one Apply per person: two runs would share configScript's .tmp
}

// applyLock is a person's Apply mutex; n (under Service.mu) counts holders and
// waiters, so the entry goes once nobody needs it.
type applyLock struct {
	sync.Mutex
	n int
}

// Effect says when a switch reaches the person's OpenCode.
type Effect int

const (
	EffectNow       Effect = iota // reloaded: the next step of every chat sees it
	EffectBusy                    // apply pending: retried once idle (RetryPending)
	EffectNextStart               // environment asleep: applied when it starts
)

func (e Effect) String() string {
	return [...]string{"now", "busy", "next start"}[e]
}

func (s *Service) Enabled() bool { return s.Dir != "" }

func (s *Service) Items() []Item {
	if !s.Enabled() {
		return nil
	}
	s.cacheMu.Lock()
	if !s.loaded {
		var err error
		s.items, err = Load(s.Dir)
		s.loaded = true
		if err != nil {
			s.Log.Warn("catalog: skipped entries", "err", err)
		}
	}
	// Callers filter in place; never expose the cached slice.
	items := slices.Clone(s.items)
	s.cacheMu.Unlock()
	for i := range items {
		if items[i].Kind == KindMCP {
			items[i].Tools = slices.DeleteFunc(slices.Clone(items[i].Tools), func(tool string) bool { return !instance.Current().ToolEnabled(tool) })
		}
	}
	return slices.DeleteFunc(items, func(it Item) bool {
		switch it.Kind {
		case KindMCP:
			return len(it.Tools) == 0
		case KindCentral:
			return !instance.Current().RemoteEnabled(it.ID)
		case KindToolbox:
			return !instance.Current().Enabled("toolbox")
		}
		return false
	})
}

// Reload invalidates the disk snapshot after catalog files change.
func (s *Service) Reload() {
	s.cacheMu.Lock()
	s.items, s.loaded = nil, false
	s.cacheMu.Unlock()
}

// On returns the person's switched-on ids.
func (s *Service) On(ctx context.Context, uid string) (map[string]bool, error) {
	ids, err := s.Store.OptIns(ctx, uid)
	on := map[string]bool{}
	for _, id := range ids {
		on[id] = true
	}
	return on, err
}

// ToolboxTool is the workspace tool that runs every toolbox item.
const ToolboxTool = "toolbox_run"

// Grants is the normal helper's Workspace-MCP filter (deny-by-default): allowed holds the tools of
// standard MCP items and of the person's switched-on optional ones, and
// ToolboxTool when a toolbox is on; off maps each tool of a switched-off
// optional item to its title in lang ("en" or German). A tool in neither is
// granted by no item, also when the catalog failed to load. A store error
// leaves only the standard tools when project scoping is disabled; scoped requests fail closed.
func (s *Service) Grants(ctx context.Context, uid, lang string) (allowed map[string]bool, off map[string]string) {
	items := s.Items()
	active, err := s.scopedItems(ctx, uid)
	on := map[string]bool{}
	if err != nil {
		s.Log.WarnContext(ctx, "catalog: project grants", "uid", uid, "err", err)
	}
	for _, it := range active {
		on[it.ID] = true
	}
	allowed, off = map[string]bool{}, map[string]string{}
	for _, it := range items {
		granted := on[it.ID] || (err != nil && !s.ProjectEnabled() && it.Tier == TierStandard)
		s.Log.DebugContext(ctx, "catalog grant", "item", it.ID, "kind", it.Kind, "tier", it.Tier, "on", on[it.ID], "granted", granted)
		switch {
		case it.Kind == KindToolbox && granted:
			allowed[ToolboxTool] = true
		case it.Kind != KindMCP:
		case granted:
			for _, t := range it.Tools {
				allowed[t] = true
			}
		default:
			for _, t := range it.Tools {
				off[t] = it.TitleDE
				if lang == "en" {
					off[t] = cmpOr(it.Title, it.TitleDE)
				}
			}
		}
	}
	return allowed, off
}

// Set switches a catalog item for one person and applies it when the
// environment runs; a busy helper gets it from RetryPending.
func (s *Service) Set(ctx context.Context, uid, id string, on bool) (Effect, error) {
	var found bool
	for _, it := range s.Items() {
		found = found || (it.ID == id && it.Tier == TierOptional)
	}
	if !found {
		return 0, ErrUnknown
	}
	if err := s.Store.SetOptIn(ctx, uid, id, on); err != nil {
		return 0, err
	}
	s.Log.InfoContext(ctx, "catalog switch", "uid", uid, "item", id, "on", on)
	return s.applyOrDefer(ctx, uid)
}

// linkScript makes the catalog symlinks in the skill folder match "$@":
// links into the catalog that are not wanted (or dangle) go, missing ones are
// created. Real folders and links elsewhere are never touched.
func linkScript() string {
	return `d=` + SkillsDir + `; c=` + MountDir() + `/skills
mkdir -p "$d" || exit 1
for l in "$d"/*; do
  [ -L "$l" ] || continue
  t=$(readlink "$l"); n=${l##*/}
  case "$t" in "$c"/*) ;; *) continue ;; esac
  keep=
  for id in "$@"; do [ "$id" = "$n" ] && [ "$t" = "$c/$n" ] && [ -d "$t" ] && keep=1; done
  [ -n "$keep" ] || rm -f -- "$l"
done
for id in "$@"; do
  [ -e "$d/$id" ] || [ -L "$d/$id" ] || [ ! -d "$c/$id" ] || ln -s "$c/$id" "$d/$id"
done
`
}

// Apply brings the running environment in line with the store: symlinks,
// then a location reload so skills and the workspace tool list are read again.
// Measured (2.0.30, runtime image, /tmp noexec): without the reload neither a
// new nor a removed symlink nor a changed tools/list is seen by an already
// loaded project; the reload is skipped while a chat runs, because it would
// cancel open questions.
func (s *Service) Apply(ctx context.Context, uid string) (Effect, error) {
	s.mu.Lock()
	l := s.applyMu[uid]
	if l == nil {
		l = &applyLock{}
		if s.applyMu == nil {
			s.applyMu = map[string]*applyLock{}
		}
		s.applyMu[uid] = l
	}
	l.n++
	s.mu.Unlock()
	l.Lock()
	defer func() {
		l.Unlock()
		s.mu.Lock()
		if l.n--; l.n == 0 {
			delete(s.applyMu, uid)
		}
		s.mu.Unlock()
	}()
	c, ok := s.Client(uid)
	if !ok || !s.Enabled() {
		s.Log.DebugContext(ctx, "catalog apply", "uid", uid, "effect", "next start", "reason", "not running")
		return EffectNextStart, nil
	}
	on, err := s.On(ctx, uid)
	if err != nil {
		return 0, err
	}
	var ids []string
	for _, it := range s.Items() {
		// standard catalog skills are linked for everyone, without an opt-in (D53 praesentation)
		if !s.ProjectEnabled() && it.Kind == KindSkill && (on[it.ID] || it.Tier == TierStandard) {
			ids = append(ids, it.ID)
		}
	}
	s.Log.DebugContext(ctx, "catalog apply", "uid", uid, "skills", ids, "on", slices.Sorted(maps.Keys(on)))
	if err := s.Shell(ctx, uid, linkScript(), nil, ids...); err != nil {
		return 0, fmt.Errorf("skill links: %w", err)
	}
	global := on
	if s.ProjectEnabled() {
		global = nil
	}
	if err := s.Shell(ctx, uid, configScript(), nil, centralConfig(s.Items(), global)); err != nil {
		return 0, fmt.Errorf("central config: %w", err)
	}
	if err := s.applyProjects(ctx, uid); err != nil {
		return 0, err
	}
	return s.reload(ctx, uid, c)
}

// configScript replaces ConfigFile with "$1" by rename, so OpenCode never
// reads half a file.
func configScript() string {
	return `f=` + ConfigFile() + `
mkdir -p "${f%/*}" && printf '%s' "$1" > "$f.tmp" && mv -f -- "$f.tmp" "$f"
`
}

// centralConfig is the person's OPENCODE_CONFIG: one remote server per
// switched-on mcp-central item, all on llm-gw. The person can edit the file;
// the gateway checks the opt-in again on every request, and the admin
// policy (mcp:<id> allow) keeps any other server out.
func centralConfig(items []Item, on map[string]bool) string {
	servers := map[string]any{}
	for _, it := range items {
		if it.Kind == KindCentral && on[it.ID] {
			servers[it.ID] = map[string]any{"type": "remote", "url": centralURL + it.ID, "oauth": false, "codemode": false,
				"timeout": map[string]int{"startup": 10000, "catalog": 15000, "execution": 60000}}
		}
	}
	b, _ := json.Marshal(map[string]any{"mcp": map[string]any{"servers": servers}})
	return string(b)
}

// OnItems returns the person's switched-on catalog items of one kind; a
// store error counts as none switched on.
func (s *Service) OnItems(ctx context.Context, uid, kind string) []Item {
	items, err := s.scopedItems(ctx, uid)
	if err != nil {
		s.Log.WarnContext(ctx, "catalog: scoped items", "uid", uid, "err", err)
		return nil
	}
	var out []Item
	for _, it := range items {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

// AssistantItems resolves operator-declared grants without reading or changing
// personal opt-ins. Callers must enforce the assistant's session and tool scope.
func (s *Service) AssistantItems(p assistants.Definition) []Item {
	sources, err := p.Sources(instance.Current().KnowledgeSources)
	if err != nil {
		return nil
	}
	var out []Item
	for _, it := range s.Items() {
		granted := it.Kind == KindSkill && slices.Contains(p.Skills, it.ID)
		for _, source := range sources {
			entry, server := source.Server()
			granted = granted || (it.Kind == KindCentral && it.ID == entry && slices.Contains(it.LiteLLM, server))
		}
		if it.Kind == KindMCP {
			it.Tools = slices.DeleteFunc(slices.Clone(it.Tools), func(tool string) bool {
				return !slices.Contains(p.MCPTools, instance.Current().MCPServer+"."+tool)
			})
			granted = len(it.Tools) > 0
		}
		if granted {
			out = append(out, it)
		}
	}
	return out
}

func (s *Service) reload(ctx context.Context, uid string, c *ocapi.Client) (Effect, error) {
	active, err := c.Active(ctx)
	if err != nil {
		return 0, err
	}
	if len(active) > 0 {
		s.Log.DebugContext(ctx, "catalog reload deferred", "reason", "agent running", "active", len(active))
		return EffectBusy, nil
	}
	locations, err := c.Locations(ctx)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, location := range locations {
		var out bytes.Buffer
		// Probe inside the container: its paths need not exist on the manager.
		if err := s.Shell(ctx, uid, `[ -d "$1" ] && printf present; exit 0`, &out, location.Directory); err != nil {
			errs = append(errs, err)
			continue
		}
		if out.Len() == 0 {
			if err := c.EvictLocation(ctx, location.Directory); err != nil {
				errs = append(errs, err)
			} else {
				s.Log.InfoContext(ctx, "catalog evicted missing location", "uid", uid, "directory", location.Directory)
			}
		}
	}
	// OpenCode settles every replacement even if another location fails.
	errs = append(errs, c.LocationReload(ctx))
	return EffectNow, errors.Join(errs...)
}

// ReloadAll applies the catalog to every running environment after the
// catalog dir changed (rsync, POST /admin/catalog/reload): idle ones now,
// busy ones from RetryPending once the helper is idle.
func (s *Service) ReloadAll(ctx context.Context) (now, later int) {
	s.Reload()
	for _, uid := range s.Running() {
		if e, err := s.applyOrDefer(ctx, uid); err == nil && e != EffectBusy {
			now++
		} else {
			later++
		}
	}
	s.Log.InfoContext(ctx, "catalog reload all", "now", now, "later", later)
	return now, later
}

// RetryPending is the reap-tick hook (lifecycle.Service.AfterReap).
func (s *Service) RetryPending(ctx context.Context) {
	s.mu.Lock()
	uids := slices.Collect(maps.Keys(s.pending))
	s.mu.Unlock()
	if len(uids) > 0 {
		s.Log.DebugContext(ctx, "catalog retry pending", "pending", uids)
	}
	for _, uid := range uids {
		s.applyOrDefer(ctx, uid)
	}
}

// applyOrDefer runs Apply; uid is done when reloaded now or asleep (the
// start hook applies). Busy or failed stays pending for the next tick.
func (s *Service) applyOrDefer(ctx context.Context, uid string) (Effect, error) {
	e, err := s.Apply(ctx, uid)
	if err != nil {
		s.Log.WarnContext(ctx, "catalog: reload", "op", "apply", "uid", uid, "err", err)
	} else {
		s.Log.InfoContext(ctx, "catalog applied", "uid", uid, "effect", e.String())
	}
	done := err == nil && e != EffectBusy
	s.mu.Lock()
	defer s.mu.Unlock()
	if done {
		delete(s.pending, uid)
	} else {
		if s.pending == nil {
			s.pending = map[string]bool{}
		}
		s.pending[uid] = true
	}
	s.Log.DebugContext(ctx, "catalog pending", "uid", uid, "done", done, "pending", len(s.pending))
	if err != nil {
		return EffectBusy, nil // the saved switch is queued, not rejected
	}
	return e, nil
}

// AfterStart applies the switches made while the environment slept and
// clears links to removed catalog skills (lifecycle hook after readiness).
func (s *Service) AfterStart(ctx context.Context, uid string) {
	s.Reload()
	if !s.Enabled() {
		return
	}
	s.applyOrDefer(ctx, uid)
}

// Skill is a skill folder as found in the person's container.
type Skill struct {
	ID            string
	TitleDE       string
	DescriptionDE string
	// English display texts (workspace/title and description; older own skills: workspace/titel-en, workspace/beschreibung-en)
	Title, Description string
	Changed            time.Time
}

// listScript prints kind, id, link target, mtime and the head of SKILL.md for
// each standard skill and each folder or link in the person's skill folder,
// every field NUL-terminated.
const listScript = `emit() {
  f="$3/SKILL.md"; [ -f "$f" ] || return 0
  printf '%s\0%s\0%s\0%s\0' "$1" "$2" "$(readlink "$3")" "$(stat -L -c %Y "$f")"
  head -c 8192 "$f"; printf '\0'
}
for d in ` + StandardDir + `/*; do [ -d "$d" ] && emit standard "${d##*/}" "$d"; done
for d in ` + SkillsDir + `/*; do
  [ -d "$d" ] || continue
  if [ -L "$d" ]; then emit link "${d##*/}" "$d"; else emit own "${d##*/}" "$d"; fi
done
exit 0
`

// Skills lists the standard skills and the person's own skills (real
// folders); catalog links are left out, the switches show them.
func (s *Service) Skills(ctx context.Context, uid string) (standard, own []Skill, err error) {
	if _, ok := s.Client(uid); !ok {
		return nil, nil, ErrNotRunning
	}
	var out bytes.Buffer
	if err := s.Shell(ctx, uid, listScript, &out); err != nil {
		return nil, nil, err
	}
	standard, own = parseList(out.String())
	return standard, own, nil
}

func parseList(out string) (standard, own []Skill) {
	f := strings.Split(out, "\x00")
	for i := 0; i+4 < len(f); i += 5 {
		kind, id, mtime, head := f[i], f[i+1], f[i+3], f[i+4]
		if kind != "standard" && kind != "own" {
			continue
		}
		fm, _ := ParseFrontmatter(head)
		sec, _ := strconv.ParseInt(mtime, 10, 64)
		sk := skillTexts(fm)
		sk.ID, sk.Changed = id, time.Unix(sec, 0)
		sk.TitleDE = cmpOr(sk.TitleDE, id)
		if kind == "standard" {
			standard = append(standard, sk)
		} else {
			own = append(own, sk)
		}
	}
	return standard, own
}

// skillTexts reads the display texts of a skill in the container: the
// English-first keys (workspace/title, workspace/title-de, workspace/description-de, English
// description), or the older German-first ones that own skills written
// before the rename still carry (workspace/titel, workspace/titel-en, workspace/beschreibung-en,
// German description).
func skillTexts(fm Frontmatter) Skill {
	m := fm.Meta
	if m["workspace/title"] != "" || m["workspace/title-de"] != "" || m["workspace/description-de"] != "" {
		en := Shown(fm.Description)
		return Skill{TitleDE: cmpOr(m["workspace/title-de"], m["workspace/title"]), DescriptionDE: cmpOr(m["workspace/description-de"], en),
			Title: m["workspace/title"], Description: en}
	}
	return Skill{TitleDE: m["workspace/titel"], DescriptionDE: Shown(fm.Description), Title: m["workspace/titel-en"], Description: m["workspace/beschreibung-en"]}
}

// ownRE: what the portal accepts as an own skill folder name in a URL.
var ownRE = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,63}$`)

const deleteScript = `d=` + SkillsDir + `/"$1"
[ -d "$d" ] && [ ! -L "$d" ] || exit 3
p=$(realpath -e -- "$d" 2>/dev/null) && [ "$p" = "$d" ] || exit 3
rm -rf -- "$d"
`

// DeleteOwn removes an own skill folder for good (portal confirmation page
// first). Catalog links are not own skills and are refused.
func (s *Service) DeleteOwn(ctx context.Context, uid, id string) (Effect, error) {
	_, ok := s.Client(uid)
	if !ok {
		return 0, ErrNotRunning
	}
	if !ownRE.MatchString(id) {
		return 0, ErrNotFound
	}
	if err := s.Shell(ctx, uid, deleteScript, nil, id); err != nil {
		return 0, err
	}
	return s.applyOrDefer(ctx, uid)
}

func MountDir() string   { return instance.Value("catalog_mount") }
func ConfigFile() string { return instance.ConfigFile() }

// PendingReloads counts capability reloads waiting for an idle runtime or retry.
func (s *Service) PendingReloads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}
