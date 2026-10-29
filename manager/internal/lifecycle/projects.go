// SPDX-License-Identifier: MIT

package lifecycle

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// CreateProject creates the folder layout of docs/adr/README.md §3.6 inside the
// container, runs git init (D11) and pins the OpenCode project id via
// .git/opencode (docs/adr/README.md §3.2). The folder is removed again on failure.
func (s *Service) CreateProject(ctx context.Context, uid string, np NewProject) error {
	return s.createProject(ctx, uid, np, assistants.Pin{})
}

func (s *Service) createProject(ctx context.Context, uid string, np NewProject, pin assistants.Pin) error {
	if !ValidProject(np.Name) {
		return ErrInvalid
	}
	if err := s.ready(ctx, uid); err != nil {
		return err
	}
	return s.createReadyProject(ctx, uid, np, pin)
}

// createReadyProject requires a running environment.
func (s *Service) createReadyProject(ctx context.Context, uid string, np NewProject, pin assistants.Pin) error {
	name, description := np.Name, np.Description
	if taken, err := s.st.NameTaken(ctx, uid, name); err != nil {
		return err
	} else if taken {
		return ErrExists
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	p := store.Project{Capabilities: np.Capabilities, Assistant: pin, ID: id.String(), UID: uid, Folder: name, Description: description, CreatedAt: s.now()}
	meta, _ := json.Marshal(map[string]any{"description": description, "created": p.CreatedAt.UTC().Format(time.RFC3339), "creator_uid": uid})
	dir := s.projectDir(name)
	l := Layout()
	err = s.sh(ctx, uid, `d=$1; { [ -e "$d" ] || [ -L "$d" ]; } && exit 4; mkdir -- "$d" || exit 1
trap 'rm -rf -- "$d"' EXIT
cd "$d" && mkdir -- "$5" "$6" "$7" "$8" && printf '%s\n' "$2" > "$8/$9" &&
printf '%s\n' "$3" > "${8}.json" && git init -q && printf '%s\n' "$4" > .git/opencode || exit 1
trap - EXIT`, nil, nil, dir, description, string(meta), p.ID, l.Inbox, l.Results, l.Work, l.ProjectMeta, l.Brief)
	if err != nil {
		return err
	}
	save := s.st.CreateProject
	if pin.ID != "" {
		save = s.st.CreateOpenCodeAssistantProject
	}
	if err := save(ctx, p); err != nil {
		s.sh(context.WithoutCancel(ctx), uid, `rm -rf -- "$1"`, nil, nil, dir)
		if errors.Is(err, store.ErrExists) {
			return ErrExists
		}
		return err
	}
	return nil
}

// Sessions lists the chats of a project; nil while the environment is stopped.
func (s *Service) Sessions(ctx context.Context, uid, project string) ([]ocapi.Session, error) {
	c, ok := s.Client(uid)
	if !ok {
		return nil, nil
	}
	return c.SessionList(ctx, s.projectDir(project), 0)
}

// Active returns the ids of sessions with a running agent; nil while stopped.
func (s *Service) Active(ctx context.Context, uid string) ([]string, error) {
	c, ok := s.Client(uid)
	if !ok {
		return nil, nil
	}
	return c.Active(ctx)
}

// MoveToTrash moves the project folder to /workspace/<Layout.Trash>/<id>
// (same mount, so a rename) unless an agent works in it. The ids of all its
// sessions are stored with the trash row: OpenCode keys sessions by directory
// only, so a purge must never look them up by the (reusable) folder name.
func (s *Service) MoveToTrash(ctx context.Context, uid, project string) error {
	p, err := s.st.ProjectByFolder(ctx, uid, project)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if p.Isolated {
		now := s.now()
		return s.st.MoveToTrash(ctx, p, newID(), now, now.Add(config.TrashRetention), nil, false, false)
	}
	if err := s.ready(ctx, uid); err != nil {
		return err
	}
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	if err := s.closeProjectCAD(ctx, uid, p.ID); err != nil {
		return err
	}
	c, ok := s.Client(uid)
	if !ok {
		return ErrNotRunning
	}
	active, err := c.Active(ctx)
	if err != nil {
		return err
	}
	sess, err := c.SessionListAll(ctx, s.projectDir(project))
	if err != nil {
		return err
	}
	keep := make([]store.TrashSession, 0, len(sess))
	for _, se := range sess {
		if slices.Contains(active, se.ID) {
			return ErrBusy
		}
		keep = append(keep, store.TrashSession{ID: se.ID, Root: se.ParentID == ""})
	}
	id := newID()
	now := s.now()
	if err := s.st.MoveToTrash(ctx, p, id, now, now.Add(config.TrashRetention), keep, false, true); err != nil {
		return err
	}
	if err := s.sh(ctx, uid, `[ -d "$1" ] && [ ! -L "$1" ] || exit 3; mkdir -p -- "$(dirname -- "$2")" && mv -T -- "$1" "$2"`,
		nil, nil, s.projectDir(project), s.trashDir(id)); err != nil {
		return err
	}
	s.evictRemovedLocation(ctx, uid, s.projectDir(project))
	return s.st.FinishPending(ctx, id, keep)
}

// Restore moves a trash item back, under newName if given. A new name also
// moves the stored sessions to the new folder; if that fails, the folder goes
// back to the trash and the item stays there.
func (s *Service) Restore(ctx context.Context, uid, id, newName string) error {
	t, err := s.st.TrashItem(ctx, uid, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	name := t.Project.Folder
	if newName != "" {
		name = newName
	}
	if def, ok := instance.Current().Assistant(t.Project.Assistant.ID); ok && !t.Project.Isolated && def.ProjectMode == "shared" && name != def.SharedFolder() {
		return ErrInvalid
	}
	if !ValidProject(name) {
		return ErrInvalid
	}
	if _, err := s.st.ProjectByFolder(ctx, uid, name); err == nil {
		return ErrExists
	}
	if t.Project.Isolated {
		return s.st.RestoreTrash(ctx, t, name, s.now())
	}
	if err := s.ready(ctx, uid); err != nil {
		return err
	}
	s.finishPending(ctx, uid)
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	if t, err = s.st.TrashItem(ctx, uid, id); err != nil {
		return err
	} else if t.Pending { // its folder has a running agent
		return ErrBusy
	}
	sess, err := s.st.TrashSessions(ctx, id)
	if err != nil {
		return err
	}
	if err := s.sh(ctx, uid, `{ [ -e "$2" ] || [ -L "$2" ]; } && exit 4; [ -d "$1" ] && [ ! -L "$1" ] || exit 3; mv -T -- "$1" "$2"`,
		nil, nil, s.trashDir(id), s.projectDir(name)); err != nil {
		return err
	}
	undo := func() error {
		bg := context.WithoutCancel(ctx)
		var err error
		if name != t.Project.Folder {
			err = s.moveSessions(bg, uid, sess, s.projectDir(t.Project.Folder))
		}
		return errors.Join(err, s.sh(bg, uid, `mv -T -- "$1" "$2"`, nil, nil, s.projectDir(name), s.trashDir(id)))
	}
	if name != t.Project.Folder {
		if err := s.moveSessions(ctx, uid, sess, s.projectDir(name)); err != nil {
			return errors.Join(err, undo())
		}
	}
	if err := s.st.RestoreTrash(ctx, t, name, s.now()); err != nil {
		if errors.Is(err, store.ErrExists) {
			err = ErrExists
		}
		return errors.Join(err, undo())
	}
	return nil
}

// RenameProject renames the project folder unless an agent works in it, then
// points its sessions at the new folder (OpenCode keys them by directory) and
// updates the store. A failed step moves folder and sessions back.
func (s *Service) RenameProject(ctx context.Context, uid, project, newName string) error {
	if !ValidProject(newName) {
		return ErrInvalid
	}
	p, err := s.st.ProjectByFolder(ctx, uid, project)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if def, ok := instance.Current().Assistant(p.Assistant.ID); ok && !p.Isolated && def.ProjectMode == "shared" {
		return ErrInvalid
	}
	if taken, err := s.st.NameTaken(ctx, uid, newName); err != nil {
		return err
	} else if taken {
		return ErrExists
	}
	if p.Isolated {
		return s.st.RenameProject(ctx, p.ID, newName)
	}
	if err := s.ready(ctx, uid); err != nil {
		return err
	}
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	if err := s.closeProjectCAD(ctx, uid, p.ID); err != nil {
		return err
	}
	c, ok := s.Client(uid)
	if !ok {
		return ErrNotRunning
	}
	active, err := c.Active(ctx)
	if err != nil {
		return err
	}
	from, to := s.projectDir(project), s.projectDir(newName)
	all, err := c.SessionListAll(ctx, from)
	if err != nil {
		return err
	}
	sess := make([]store.TrashSession, 0, len(all))
	for _, se := range all {
		if slices.Contains(active, se.ID) {
			return ErrBusy
		}
		sess = append(sess, store.TrashSession{ID: se.ID})
	}
	if err := s.sh(ctx, uid, `{ [ -e "$2" ] || [ -L "$2" ]; } && exit 4; [ -d "$1" ] && [ ! -L "$1" ] || exit 3; mv -T -- "$1" "$2"`,
		nil, nil, from, to); err != nil {
		return err
	}
	undo := func() {
		bg := context.WithoutCancel(ctx)
		s.moveSessions(bg, uid, sess, from)
		s.sh(bg, uid, `mv -T -- "$1" "$2"`, nil, nil, to, from)
	}
	if err := s.moveSessions(ctx, uid, sess, to); err != nil {
		undo()
		return err
	}
	if err := s.st.RenameProject(ctx, p.ID, newName); err != nil {
		undo()
		if errors.Is(err, store.ErrExists) {
			return ErrExists
		}
		return err
	}
	s.touchProject(ctx, uid, newName, s.now())
	s.evictRemovedLocation(ctx, uid, from)
	return nil
}

// A removed folder must not leave a cached graph for the next global reload.
// Chats stay in OpenCode until purge; catalog apply retries stale-cache cleanup.
func (s *Service) evictRemovedLocation(ctx context.Context, uid, dir string) {
	if c, ok := s.Client(uid); ok {
		if err := c.EvictLocation(ctx, dir); err != nil {
			s.log.WarnContext(ctx, "location eviction failed", "uid", uid, "directory", dir, "err", err)
		}
	}
}

// moveSessions points the stored sessions at dir (POST /api/session/{id}/move);
// 404 = session already gone.
func (s *Service) moveSessions(ctx context.Context, uid string, sess []store.TrashSession, dir string) error {
	c, ok := s.Client(uid)
	if !ok {
		return ErrNotRunning
	}
	for _, se := range sess {
		if err := c.SessionMove(ctx, se.ID, dir); err != nil && !ocapi.IsStatus(err, 404) {
			return err
		}
	}
	return nil
}

// Purge deletes a trash item for good: the folder, then every stored session
// (404 = already gone). Session delete errors are logged; once the folder is
// gone, the trash row must go too.
func (s *Service) Purge(ctx context.Context, uid, id string) error {
	t, err := s.st.TrashItem(ctx, uid, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if t.Project.Isolated {
		return s.st.DeleteTrash(ctx, uid, id)
	}
	if err := s.ready(ctx, uid); err != nil {
		return err
	}
	s.finishPending(ctx, uid) // a pending item's folder is still in /workspace
	return s.purge(ctx, uid, id)
}

// purge is Purge on a running environment; it does not start or touch it.
// A pending item waits: its folder still sits in /workspace.
func (s *Service) purge(ctx context.Context, uid, id string) error {
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	if t, err := s.st.TrashItem(ctx, uid, id); err != nil {
		return err
	} else if t.Project.Isolated {
		return s.st.DeleteTrash(ctx, uid, id)
	} else if t.Pending {
		return ErrBusy
	}
	sess, err := s.st.TrashSessions(ctx, id)
	if err != nil {
		return err
	}
	c, ok := s.Client(uid)
	if !ok {
		return ErrNotRunning
	}
	if err := s.sh(ctx, uid, `rm -rf -- "$1"`, nil, nil, s.trashDir(id)); err != nil {
		return err
	}
	s.evictRemovedLocation(ctx, uid, s.trashDir(id))
	for _, se := range sess {
		if err := c.SessionDelete(ctx, se.ID); err != nil && !ocapi.IsStatus(err, 404) {
			s.log.WarnContext(ctx, "purged session delete failed", "uid", uid, "trash", id, "session", se.ID, "err", err)
		}
	}
	return s.st.DeleteTrash(ctx, uid, id)
}

// purgeExpired purges the trash items of uid past their date. Reap calls it
// for running environments only and it neither starts nor touches them: an
// expired item waits for the person's next start, and a failing purge never
// keeps a container alive.
func (s *Service) purgeExpired(ctx context.Context, uid string) {
	ts, err := s.st.Trash(ctx, uid)
	if err != nil {
		s.log.WarnContext(ctx, "trash list failed", "err", err)
		return
	}
	for _, t := range ts {
		if t.PurgeAfter.After(s.now()) {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, time.Minute)
		if err := s.purge(pctx, uid, t.ID); err != nil {
			s.log.WarnContext(ctx, "auto purge failed", "trash", t.ID, "err", err)
		}
		cancel()
	}
}

// CreateSession creates a chat in the project (location in the JSON body),
// queues prompt if given and returns the OpenCode deep link.
func (s *Service) CreateSession(ctx context.Context, uid, project, prompt string) (string, error) {
	if !ValidProject(project) {
		return "", ErrInvalid
	}
	c, ok := s.Client(uid)
	if !ok {
		return "", ErrNotRunning
	}
	u, err := s.st.UserByUID(ctx, uid)
	if err != nil {
		return "", err
	}
	p, err := s.st.ProjectByFolder(ctx, uid, project)
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if s.PrepareCapabilities != nil && p.Assistant.ID == "" {
		if err := s.PrepareCapabilities(ctx, uid, p.ID); err != nil {
			return "", err
		}
	}
	agent := ""
	var sourceServers []string
	if p.Assistant.ID != "" {
		def, exists := instance.Current().Assistant(p.Assistant.ID)
		if p.Isolated || !exists || !def.Enabled || instance.Current().AssistantPin(def) != p.Assistant {
			return "", ErrInvalid
		}
		if err := s.st.SharedRuntimeAccess(ctx, uid); err != nil {
			return "", err
		}
		agent = def.AgentName()
		sources, err := def.Sources(instance.Current().KnowledgeSources)
		if err != nil {
			return "", err
		}
		for _, source := range sources {
			entry, _ := source.Server()
			if !slices.Contains(sourceServers, entry) {
				sourceServers = append(sourceServers, entry)
			}
		}
		if len(def.MCPTools) > 0 {
			sourceServers = append(sourceServers, instance.Current().MCPServer)
		}
		body, err := def.RuntimeConfig(p.ID, sources)
		if err != nil {
			return "", err
		}
		var changed bytes.Buffer
		if err := s.Shell(ctx, uid, `printf '%s\n' "$2" | cmp -s - "$1/opencode.json" || printf changed`, &changed, s.projectDir(project), string(body)); err != nil {
			return "", err
		}
		if changed.Len() > 0 {
			active, err := c.Active(ctx)
			if err != nil {
				return "", err
			}
			if len(active) > 0 {
				return "", ErrBusy
			}
			if err := c.EvictLocation(ctx, s.projectDir(project)); err != nil {
				return "", err
			}
			// This manager-owned config belongs to the assistant project, never the user config.
			if err := s.Shell(ctx, uid, `f=$(mktemp "$1/.profile-config.XXXXXX") || exit 1
trap 'rm -f -- "$f"' EXIT
			printf '%s\n' "$2" > "$f" && mv -f -- "$f" "$1/opencode.json"`, nil, s.projectDir(project), string(body)); err != nil {
				return "", err
			}
		}
	}
	s.Touch(uid)
	s.touchProject(ctx, uid, project, s.now())
	se, err := c.SessionCreate(ctx, s.projectDir(project), "", agent)
	if err != nil {
		return "", err
	}
	if prompt != "" {
		// Wait for the assistant sources (or normal helper tools) before the first prompt.
		if err := s.waitMCP(ctx, c, s.projectDir(project), sourceServers...); err != nil {
			s.log.WarnContext(ctx, "prompt sent before MCP sources connected", "project", project, "err", err)
		}
		if err := c.SessionPrompt(ctx, se.ID, prompt); err != nil {
			return "", err
		}
	}
	if err := s.settle(ctx, c, s.projectDir(project), se.ID, prompt != ""); err != nil {
		s.log.WarnContext(ctx, "chat opened before opencode settled", "project", project, "session", se.ID, "err", err)
	}
	return s.DeepLink(u.Slug, se.ID), nil
}

// Waits of settle; tests shorten them.
var settleTimeout, settlePoll = 20 * time.Second, 250 * time.Millisecond

var errPromptPending = errors.New("prompt still in the inbox")

// settle holds the redirect until OpenCode has loaded the folder and a sent
// prompt is a stored message. Plugin activation alone does not guarantee
// that the runner has consumed the queued prompt.
func (s *Service) settle(ctx context.Context, c *ocapi.Client, dir, id string, prompted bool) error {
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	for {
		err := c.LoadLocation(ctx, dir)
		if err == nil && prompted {
			var types []string
			if types, err = c.MessageTypes(ctx, id); err == nil && !slices.Contains(types, "user") {
				err = errPromptPending
			}
		}
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(settlePoll):
		}
	}
}

var errMCPPending = errors.New("mcp not connected yet")

// mcpToolsGrace is the wait after "connected" until the tool registry has the MCP tools.
// shortcut: fixed delay, OpenCode exposes no tool list; replace once it does.
var mcpToolsGrace = 2 * time.Second

func (s *Service) waitMCP(ctx context.Context, c *ocapi.Client, dir string, servers ...string) error {
	if len(servers) == 0 && instance.Current().HasBuiltinTools() {
		servers = []string{instance.Current().MCPServer}
	}
	if len(servers) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, settleTimeout)
	defer cancel()
	for {
		var err error
		for _, server := range servers {
			var connected bool
			connected, err = c.MCPConnected(ctx, dir, server)
			if err == nil && !connected {
				err = errMCPPending
			}
			if err != nil {
				break
			}
		}
		if err == nil {
			// OpenCode sets "connected" before its tool registry picks the MCP tools
			// up from the ToolsChanged event (core/src/mcp/index.ts); a run started in
			// that window has no workspace_* tools in its first step.
			select {
			case <-ctx.Done():
			case <-time.After(mcpToolsGrace):
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(settlePoll):
		}
	}
}

// DeepLink is /server/<base64url(origin)>/session/<id>, the encoding of the OpenCode web UI.
func (s *Service) DeepLink(slug, sessionID string) string {
	origin := "https://" + slug + "." + s.cfg.Domain
	return origin + "/server/" + base64.RawURLEncoding.EncodeToString([]byte(origin)) + "/session/" + sessionID
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type startState struct {
	since time.Time
	done  bool
	err   error
	errID string
	full  bool // ErrFull: no error id, the next page load tries again
}

// StartAsync triggers a start and returns at once; StartState reports progress.
func (s *Service) StartAsync(rctx context.Context, uid string) {
	s.mu.Lock()
	if st := s.starts[uid]; s.background.Err() != nil || (st != nil && !st.done) || s.run[uid] != nil {
		s.mu.Unlock()
		return
	}
	st := &startState{since: s.now()}
	s.starts[uid] = st
	s.workers.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.workers.Done()
		ctx, cancel := context.WithTimeout(logx.With(s.background, logx.FromContext(rctx)), 30*time.Second)
		defer cancel()
		_, err := s.EnsureRunning(ctx, uid)
		s.mu.Lock()
		st.done, st.err, st.full = true, err, errors.Is(err, ErrFull)
		if err != nil && !st.full {
			st.errID = newID()[:8]
			s.log.ErrorContext(ctx, "start failed", "op", "start", "uid", uid, "error_id", st.errID, "err", err)
		}
		s.mu.Unlock()
	}()
}

// StartState returns the last portal-triggered start: starting, failed with
// an error id, or refused because all places were taken (full).
func (s *Service) StartState(uid string) (starting bool, since time.Time, errID string, full bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.starts[uid]
	if st == nil || s.run[uid] != nil {
		return false, time.Time{}, "", false
	}
	return !st.done, st.since, st.errID, st.full
}
