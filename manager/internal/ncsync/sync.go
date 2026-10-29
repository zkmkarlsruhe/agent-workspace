// SPDX-License-Identifier: MIT

// Package ncsync links a project to a folder in the person's Nextcloud
// ("Agent Workspace/<projekt>/"): every file the helper writes in results/
// (optionally the whole project except work/) is pushed there a few seconds
// later, so the person watches it change in Nextcloud. Triggers are the
// person's OpenCode event stream (tool results, file events) and a slow poll;
// both only mark the person dirty, a debounced pass compares the project with
// the stored state and uploads what changed. Uploads carry If-Match with the
// ETag of the manager's own last upload, so an edit made in Nextcloud
// meanwhile is never overwritten: the helper's version then lands next to it
// as "<name> (Helfer).<ext>". A version the person made (pulled, or there
// before) is never overwritten at all (D58).
package ncsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ncguard"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Root is the folder in every person's Nextcloud that holds linked projects.
func Root() string { return ncguard.Root() }

// RootText is rendered in static tool descriptions after language selection.
const RootText = "[[nextcloud_root]]"

// MetaFile is written to <project>/.project/ while the project is linked or
// has collections; the admin config lists it under "instructions".
const MetaFile = "nextcloud.md"

var (
	ErrNotConnected = errors.New("nextcloud not connected")
	ErrNotLinked    = errors.New("project not linked")
	ErrNotFolder    = errors.New("not a folder in nextcloud")
)

// Files is the part of lifecycle.Service the sync uses.
type Files interface {
	ScanFiles(ctx context.Context, uid, project, sub string) ([]lifecycle.File, error)
	ReadFile(ctx context.Context, uid, project, rel string) (io.ReadCloser, error)
	ReplaceFile(ctx context.Context, uid, project, rel string, r io.Reader) error
	Upload(ctx context.Context, uid, project, filename string, r io.Reader) (string, error)
	WriteMeta(ctx context.Context, uid, project, name, content string) error
	RunningUIDs() []string
	Client(uid string) (*ocapi.Client, bool)
}

type Service struct {
	DAV   *DAV
	Store *store.Store
	Files Files
	// Creds returns the person's login; any error = not connected.
	Creds func(ctx context.Context, uid string) (Creds, error)
	// Expired is told about a 401 (portal: "Verbindung abgelaufen").
	Expired func(ctx context.Context, uid string)
	// Consented reports whether uid accepted the current notice; the
	// background sync runs only then (nil = never).
	Consented func(ctx context.Context, uid string) (bool, error)
	Log       *slog.Logger

	// Debounce is the quiet time after the last trigger and the minimum age
	// of a file before it is pushed (a file still being written is not taken).
	Debounce time.Duration
	// MaxWait caps the debounce while triggers keep coming.
	MaxWait time.Duration
	// Poll marks every running linked person dirty this often (fallback for
	// writes the event stream does not show, e.g. a script run in the shell).
	Poll time.Duration
	now  func() time.Time

	mu      sync.Mutex
	due     map[string]time.Time // uid -> pass due
	first   map[string]time.Time // uid -> first trigger since the last pass
	busy    map[string]bool
	watch   map[string]context.CancelFunc
	locks   map[string]*sync.Mutex // per person: pass, pull and link never overlap
	lastPol time.Time
}

func (s *Service) init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.due == nil {
		s.due, s.first, s.busy = map[string]time.Time{}, map[string]time.Time{}, map[string]bool{}
		s.watch, s.locks = map[string]context.CancelFunc{}, map[string]*sync.Mutex{}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.Debounce == 0 {
		s.Debounce = 2 * time.Second
	}
	if s.MaxWait == 0 {
		s.MaxWait = 10 * time.Second
	}
	if s.Poll == 0 {
		s.Poll = 30 * time.Second
	}
}

func (s *Service) lock(uid string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.locks[uid]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[uid] = l
	}
	return l
}

// Kick marks a person dirty: a pass runs after Debounce without a new kick,
// at the latest MaxWait after the first one.
func (s *Service) Kick(uid string) {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if _, ok := s.first[uid]; !ok {
		s.first[uid] = now
	}
	due := now.Add(s.Debounce)
	if cap := s.first[uid].Add(s.MaxWait); cap.Before(due) {
		due = cap
	}
	s.due[uid] = due
}

// Loop runs passes, the poll and the event watchers until ctx ends.
func (s *Service) Loop(ctx context.Context) {
	s.init()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			for _, cancel := range s.watch {
				cancel()
			}
			s.mu.Unlock()
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// Tick is one round of Loop: watchers and poll, then the due passes.
func (s *Service) Tick(ctx context.Context) {
	s.init()
	if s.now().Sub(s.lastPol) >= s.Poll {
		s.lastPol = s.now()
		s.reconcile(ctx)
	}
	s.mu.Lock()
	var run []string
	for uid, at := range s.due {
		if !s.busy[uid] && !at.After(s.now()) {
			run = append(run, uid)
			s.busy[uid] = true
			delete(s.due, uid)
			delete(s.first, uid)
		}
	}
	s.mu.Unlock()
	for _, uid := range run {
		go func() {
			pctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
			again := s.Pass(pctx, uid)
			cancel()
			s.mu.Lock()
			s.busy[uid] = false
			s.mu.Unlock()
			if again {
				s.Kick(uid)
			}
		}()
	}
}

// reconcile starts an event watcher for every running person with a linked
// project, stops the others, and marks those people dirty (the poll).
func (s *Service) reconcile(ctx context.Context) {
	want := map[string]bool{}
	for _, uid := range s.Files.RunningUIDs() {
		if ls, err := s.Store.NCLinks(ctx, uid); err == nil && len(ls) > 0 && s.consented(ctx, uid) {
			want[uid] = true
			s.Kick(uid)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for uid, cancel := range s.watch {
		if !want[uid] {
			cancel()
			delete(s.watch, uid)
		}
	}
	for uid := range want {
		if s.watch[uid] == nil {
			wctx, cancel := context.WithCancel(ctx)
			s.watch[uid] = cancel
			go s.follow(wctx, uid)
		}
	}
}

// triggers are the event types after which a file in the project may have changed.
var triggers = []string{"session.tool.success", "filesystem.changed", "session.idle"}

// follow reads the person's event stream and kicks a pass on every trigger;
// it reconnects until ctx ends (reconcile cancels it when the person stops).
func (s *Service) follow(ctx context.Context, uid string) {
	for ctx.Err() == nil {
		c, ok := s.Files.Client(uid)
		if ok {
			err := c.Events(ctx, func(e ocapi.Event) {
				if slices.Contains(triggers, e.Type) && (e.Location.Directory == "" || strings.HasPrefix(e.Location.Directory, "/workspace/")) {
					s.Kick(uid)
				}
			})
			if ctx.Err() == nil {
				s.Log.DebugContext(ctx, "ncsync: event stream ended", "uid", uid, "err", err)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// Pass pushes the changed files of every linked project of uid. It reports
// true when a file was too fresh to take, so the caller kicks again.
func (s *Service) Pass(ctx context.Context, uid string) (again bool) {
	s.init()
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	ctx = logx.With(ctx, logx.Fields{UID: uid})
	links, err := s.Store.NCLinks(ctx, uid)
	if err != nil || len(links) == 0 || !s.consented(ctx, uid) {
		s.Log.DebugContext(ctx, "ncsync: pass skipped", "links", len(links), "err", err)
		return false // no consent: the pending work is dropped, reconcile no longer kicks
	}
	c, err := s.Creds(ctx, uid)
	if err != nil {
		s.Log.DebugContext(ctx, "ncsync: pass skipped", "reason", "not connected", "err", err)
		return false // not connected: the portal shows it; nothing to retry until a new login
	}
	for _, link := range links {
		if !s.consented(ctx, uid) { // revoked or a new notice during the pass
			return false
		}
		fresh, err := s.syncProject(ctx, uid, c, link)
		if err != nil {
			s.Log.WarnContext(ctx, "ncsync: pass", "op", "sync project", "uid", uid, "project", link.Folder, "project_id", link.ProjectID, "status", HTTPStatus(err), "err", err)
			if HTTPStatus(err) == 401 {
				if s.Expired != nil {
					s.Expired(ctx, uid)
				}
				return false
			}
		}
		again = again || fresh
	}
	return again
}

func (s *Service) consented(ctx context.Context, uid string) bool {
	if s.Consented == nil {
		return false
	}
	ok, err := s.Consented(ctx, uid)
	return err == nil && ok
}

// Folder is the Nextcloud folder of a project.
func Folder(project string) string { return Root() + "/" + project }

// ncRel maps a project path to its path inside the linked folder: the results
// folder is the folder itself, the rest of the project (with syncAll) keeps its path.
func ncRel(rel string, syncAll bool) (string, bool) {
	l := lifecycle.Layout()
	if r, ok := strings.CutPrefix(rel, l.Results+"/"); ok {
		return r, true
	}
	if !syncAll || strings.HasPrefix(rel, l.Work+"/") {
		return "", false
	}
	return rel, true
}

// projectRel is the reverse of ncRel for a file that appeared in Nextcloud.
func projectRel(nc string, syncAll bool) string {
	l := lifecycle.Layout()
	if syncAll && strings.HasPrefix(nc, l.Inbox+"/") {
		return nc
	}
	return l.Results + "/" + nc
}

// followRename moves the Nextcloud folder after a rename of the project.
func (s *Service) followRename(ctx context.Context, uid string, c Creds, link *store.NCLink) error {
	if link.NCFolder == link.Folder {
		return nil
	}
	err := s.DAV.Move(ctx, c, Folder(link.NCFolder), Folder(link.Folder))
	s.Log.InfoContext(ctx, "ncsync: folder renamed", "from", link.NCFolder, "to", link.Folder, "status", HTTPStatus(err), "err", err)
	switch HTTPStatus(err) {
	case 0:
		if err != nil {
			return err
		}
	case 404: // old folder gone in Nextcloud: the next upload creates the new one
	case 412:
		return fmt.Errorf("rename: %s exists", Folder(link.Folder))
	default:
		return err
	}
	link.NCFolder = link.Folder
	if err := s.Store.SetNCFolder(ctx, link.ProjectID, link.Folder); err != nil {
		return err
	}
	s.writeMeta(ctx, uid, link.Folder)
	return nil
}

func (s *Service) syncProject(ctx context.Context, uid string, c Creds, link store.NCLink) (fresh bool, err error) {
	if err := s.followRename(ctx, uid, c, &link); err != nil {
		note := tr(s.english(ctx, uid), "The folder in Nextcloud could not be renamed: “%s” already exists there.", Folder(link.Folder))
		s.Store.NCSynced(ctx, link.ProjectID, time.Time{}, note)
		return false, err
	}
	sub := lifecycle.Layout().Results
	if link.All {
		sub = ""
	}
	files, err := s.Files.ScanFiles(ctx, uid, link.Folder, sub)
	if err != nil {
		return false, err
	}
	state, err := s.Store.NCFiles(ctx, link.ProjectID)
	if err != nil {
		return false, err
	}
	var pushed int
	var notes []string
	made := map[string]bool{}
	for _, f := range files {
		nc, ok := ncRel(f.Path, link.All)
		if !ok {
			continue
		}
		old, known := state[f.Path]
		if known && old.Size == f.Size && old.MTime == f.Modified.UnixMilli() {
			continue
		}
		if s.now().Sub(f.Modified) < s.Debounce {
			s.Log.DebugContext(ctx, "ncsync: file", "project", link.Folder, "path", f.Path, "action", "wait", "reason", "too fresh")
			fresh = true
			continue
		}
		if !s.consented(ctx, uid) {
			return false, nil
		}
		dst := path.Join(Folder(link.Folder), nc)
		if dir := path.Dir(dst); !made[dir] {
			if err := s.DAV.Mkdirs(ctx, c, dir); err != nil {
				return fresh, err
			}
			made[dir] = true
		}
		cp, err := s.push(ctx, uid, c, link, f, dst, old)
		if err != nil {
			s.Log.WarnContext(ctx, "ncsync: file", "op", "upload", "project", link.Folder, "path", f.Path, "status", HTTPStatus(err), "err", err)
			return fresh, err
		}
		action := map[bool]string{true: "upload", false: "update"}[!known]
		if cp != "" {
			action = "conflict copy " + path.Base(cp)
		}
		s.Log.DebugContext(ctx, "ncsync: file", "project", link.Folder, "path", f.Path, "action", action, "bytes", f.Size, "if_match", old.Own)
		if cp != "" {
			notes = append(notes, conflictNote(s.english(ctx, uid), dst, cp))
		}
		pushed++
	}
	if pushed > 0 {
		err = s.Store.NCSynced(ctx, link.ProjectID, s.now(), strings.Join(notes, " "))
	}
	return fresh, err
}

// push uploads one file. It overwrites only a version the manager wrote
// itself (old.Own, If-Match on its ETag); otherwise, and on a refused
// precondition (changed in Nextcloud), the helper's version goes to a new
// "<name> (Helfer).<ext>" (never over an existing file, which the person may
// have edited) and the stored state keeps the old ETag, so the conflict
// stands until the person fetches her version. cp is the copy's path.
func (s *Service) push(ctx context.Context, uid string, c Creds, link store.NCLink, f lifecycle.File, dst string, old store.NCFile) (cp string, err error) {
	open := func() (io.ReadCloser, error) { return s.Files.ReadFile(ctx, uid, link.Folder, f.Path) }
	rc, err := open()
	if err != nil {
		return "", err
	}
	ifMatch := ""
	if old.Own {
		ifMatch = old.ETag
	}
	newTag, err := s.DAV.Put(ctx, c, dst, rc, ifMatch, f.Modified)
	rc.Close()
	next := store.NCFile{Rel: f.Path, Size: f.Size, MTime: f.Modified.UnixMilli(), ETag: newTag, Own: newTag != ""}
	if HTTPStatus(err) == 412 {
		for n := 1; ; n++ {
			if n > 50 {
				return "", fmt.Errorf("%s: no free name for the helper's copy", dst)
			}
			cp = helperName(dst, n)
			if rc, err = open(); err != nil {
				return "", err
			}
			_, err = s.DAV.Put(ctx, c, cp, rc, "", f.Modified)
			rc.Close()
			if HTTPStatus(err) != 412 {
				break
			}
		}
		if err != nil {
			return "", err
		}
		next.ETag, next.Own = old.ETag, old.Own
	} else if err != nil {
		return "", err
	}
	// no ETag from the PUT: none is stored (never one read back later, which
	// may already be the person's next save), so the next push makes a copy
	return cp, s.Store.PutNCFile(ctx, link.ProjectID, next)
}

// english: the portal notes are stored as text, in the person's portal language.
func (s *Service) english(ctx context.Context, uid string) bool {
	l, _ := s.Store.Lang(ctx, uid)
	return l == "en"
}

func conflictNote(en bool, dst, cp string) string {
	return tr(en, "“%s” was changed in Nextcloud. The helper’s version is next to it as “%s”; “Fetch from Nextcloud” brings your version into the project.",
		path.Base(dst), path.Base(cp))
}

// helperName is "<dir>/<stem> (Helfer)<ext>", from n = 2 on "(Helfer n)".
func helperName(p string, n int) string {
	ext := path.Ext(p)
	tag := " (Helfer)"
	if n > 1 {
		tag = fmt.Sprintf(" (Helfer %d)", n)
	}
	return strings.TrimSuffix(p, ext) + tag + ext
}
