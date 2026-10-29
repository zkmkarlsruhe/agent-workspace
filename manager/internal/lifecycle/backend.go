// SPDX-License-Identifier: MIT

package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// The methods and types in this file mirror portal.Backend (manager/internal/
// portal/backend.go on the portal branch) field by field, so the post-merge
// adapter is plain type conversion plus error mapping (see docs/INSTALL.md).

const (
	StatusNone    = ""
	StatusWorking = "arbeitet"
	StatusWaiting = "wartet"
	StatusDone    = "fertig"
	StatusFailed  = "fehler"
)

type Project struct {
	Isolated     bool
	Assistant    assistants.Pin
	Name         string
	Description  string
	Created      time.Time
	LastActivity time.Time
	Status       string
	Chats        int
	SizeBytes    int64
	CostEUR      float64 // last 30 days
	CO2MinG      float64 // last 30 days
	CO2MaxG      float64
	Recent       []Chat    // all chats, newest first (project list)
	TrashAt      time.Time // the inactivity rule moves it to the trash then
}

type ProjectDetail struct {
	Project
	ChatList      []Chat
	ArchivedChats []Chat
}

type Chat struct {
	Archived bool
	ID       string
	Title    string
	Updated  time.Time
	Status   string
	CostEUR  float64 // from the gateway, all time, subagents included
	CO2MinG  float64
	CO2MaxG  float64
	Link     string
}

type NewProject struct {
	Capabilities []string
	Name         string
	Description  string
}

type File struct {
	Name     string
	Path     string // relative to the project, set by Tree
	Dir      bool
	Size     int64
	Modified time.Time
}

type TrashItem struct {
	ID        string
	Name      string
	DeletedAt time.Time
	PurgeAt   time.Time
	SizeBytes int64
	Chats     int
	Auto      bool // moved by the inactivity rule
}

type UsageRow struct {
	Project string
	Trashed bool // the project is in the trash
	CostEUR float64
	CO2MinG float64
	CO2MaxG float64
	Chats   int // chats with usage in the period
}

type EnvState string

const (
	EnvRunning  EnvState = "running"
	EnvStopped  EnvState = "stopped"
	EnvStarting EnvState = "starting"
	EnvError    EnvState = "error"
	EnvFull     EnvState = "full" // OC_MAX_RUNNING reached, not started
)

type EnvStatus struct {
	State      EnvState
	Since      time.Time
	DiskUsed   int64
	DiskQuota  int64
	ErrorID    string
	ActiveRuns []string
}

// chatInfo returns the chats of a project and whether a question waits for
// the person; empty while the environment is stopped. Cost and CO2 come from
// the gateway's usage events, so portal and gateway agree (OpenCode's own
// cost is USD from the price table).
func (s *Service) chatInfo(ctx context.Context, p store.Project, slug string, active []string) ([]Chat, bool, error) {
	if p.Isolated {
		sessions, err := s.st.AssistantSessions(ctx, p.UID, p.ID)
		var chats []Chat
		for _, se := range sessions {
			chats = append(chats, Chat{ID: se.ID, Title: p.Description, Updated: se.Updated, Link: "/support/" + se.ID})
		}
		return chats, false, err
	}
	c, ok := s.Client(p.UID)
	if !ok {
		return nil, false, nil
	}
	dir := s.projectDir(p.Folder)
	sess, err := c.SessionListAll(ctx, dir)
	if err != nil {
		return nil, false, err
	}
	used, err := s.st.SessionUsage(ctx, p.UID, p.ID, time.Time{})
	if err != nil {
		return nil, false, err
	}
	bySession := map[string]store.UsageRow{}
	for _, u := range used {
		bySession[u.SessionID] = u
	}
	var chats []Chat
	for _, se := range sess {
		if se.ParentID != "" {
			continue // subagent sessions are not chats of the person
		}
		u := bySession[se.ID]
		chats = append(chats, Chat{ID: se.ID, Title: se.Title, Updated: sessionTime(se), Archived: se.Time.Archived != nil,
			Status: chatStatus(se, active), CostEUR: u.CostEUR, CO2MinG: u.CO2LoG, CO2MaxG: u.CO2HiG,
			Link: s.DeepLink(slug, se.ID)})
	}
	forms, err := c.Forms(ctx, dir)
	if err != nil {
		return chats, false, err
	}
	perms, err := c.PermissionRequests(ctx, dir)
	return chats, len(forms)+len(perms) > 0, err
}

// sessionTime is the last update of a session, else its creation, else zero:
// time.UnixMilli(0) is 1970, not IsZero, and would show as "01.01.1970".
func sessionTime(se ocapi.Session) time.Time {
	for _, ms := range []float64{se.Time.Updated, se.Time.Created} {
		if ms > 0 {
			return time.UnixMilli(int64(ms))
		}
	}
	return time.Time{}
}

func chatStatus(se ocapi.Session, active []string) string {
	switch {
	case slices.Contains(active, se.ID):
		return StatusWorking
	case se.Outcome == "failed":
		return StatusFailed
	case se.Outcome == "succeeded":
		return StatusDone
	}
	return StatusNone
}

func projectStatus(chats []Chat, waiting bool) (string, time.Time) {
	var last time.Time
	st := StatusNone
	for _, c := range chats {
		if c.Status == StatusWorking {
			st = StatusWorking
		}
		if c.Updated.After(last) {
			last = c.Updated
			if st != StatusWorking {
				st = c.Status
			}
		}
	}
	if waiting {
		st = StatusWaiting
	}
	return st, last
}

func (s *Service) costByProject(ctx context.Context, uid string, since time.Time) (map[string]store.UsageRow, error) {
	rows, err := s.st.Usage(ctx, uid, since)
	m := map[string]store.UsageRow{}
	for _, r := range rows {
		m[r.ProjectID] = r
	}
	return m, err
}

func (s *Service) project(ctx context.Context, p store.Project, slug string, active []string, cost map[string]store.UsageRow) (ProjectDetail, error) {
	chats, waiting, err := s.chatInfo(ctx, p, slug, active)
	var archived []Chat
	for _, ch := range chats {
		if ch.Archived {
			archived = append(archived, ch)
		}
	}
	chats = slices.DeleteFunc(chats, func(ch Chat) bool { return ch.Archived })
	st, last := projectStatus(chats, waiting)
	if last.After(p.LastActivity) {
		s.touchProject(ctx, p.UID, p.Folder, last) // chats exist only while running; the rule runs on the store
	} else {
		last = p.LastActivity
	}
	return ProjectDetail{Project: Project{Isolated: p.Isolated, Assistant: p.Assistant, Name: p.Folder, Description: p.Description, Created: p.CreatedAt,
		LastActivity: last, TrashAt: last.Add(config.ProjectIdleTrash), Status: st, Chats: len(chats) + len(archived), CostEUR: cost[p.ID].CostEUR,
		CO2MinG: cost[p.ID].CO2LoG, CO2MaxG: cost[p.ID].CO2HiG}, ChatList: chats, ArchivedChats: archived}, err
}

// sizes returns du bytes per path with one exec in the running container and
// never starts it; nil when stopped.
// shortcut: sizes only while the environment runs (stopped shows none); the
// root helper could du per path if sizes are wanted for stopped environments.
func (s *Service) sizes(ctx context.Context, uid string, dirs []string) map[string]int64 {
	if len(dirs) == 0 || s.get(uid) == nil {
		return nil
	}
	var out bytes.Buffer
	// du exits 1 if a path vanished in between; the others are still printed
	if err := s.sh(ctx, uid, `du -s --block-size=1 -- "$@" 2>/dev/null; exit 0`, nil, &out, dirs...); err != nil {
		s.log.WarnContext(ctx, "sizes unavailable", "err", err)
		return nil
	}
	m := map[string]int64{}
	for line := range strings.Lines(out.String()) {
		n, p, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
		if v, err := strconv.ParseInt(n, 10, 64); ok && err == nil {
			m[p] = v
		}
	}
	return m
}

// ListProjects lists the projects, sorted by last activity. Chat counts,
// status and sizes need the running environment; when stopped they stay empty.
func (s *Service) ListProjects(ctx context.Context, uid string) ([]Project, error) {
	u, err := s.st.UserByUID(ctx, uid)
	if err != nil {
		return nil, err
	}
	ps, err := s.st.Projects(ctx, uid)
	if err != nil {
		return nil, err
	}
	cost, err := s.costByProject(ctx, uid, s.now().Add(-30*24*time.Hour))
	if err != nil {
		return nil, err
	}
	// The chats come from the running OpenCode. The list never waits for a
	// start: it triggers one in the background and shows the projects without
	// chats meanwhile (the start page polls until the environment runs). After
	// a failed start it does not retry, so the page shows the error.
	if _, _, errID, _ := s.StartState(uid); errID == "" && (len(ps) == 0 || slices.ContainsFunc(ps, func(p store.Project) bool { return !p.Isolated })) {
		s.StartAsync(ctx, uid) // no-op while running or starting
	}
	active, _ := s.Active(ctx, uid)
	dirs := make([]string, len(ps))
	for i, p := range ps {
		dirs[i] = s.projectDir(p.Folder)
	}
	size := s.sizes(ctx, uid, dirs)
	out := make([]Project, 0, len(ps))
	for i, p := range ps {
		d, err := s.project(ctx, p, u.Slug, active, cost)
		if err != nil {
			s.log.WarnContext(ctx, "chat status unavailable", "project", p.Folder, "err", err)
		}
		d.SizeBytes = size[dirs[i]]
		recent := slices.Clone(d.ChatList)
		slices.SortStableFunc(recent, func(a, b Chat) int { return b.Updated.Compare(a.Updated) })
		d.Recent = recent
		out = append(out, d.Project)
	}
	slices.SortStableFunc(out, func(a, b Project) int { return b.LastActivity.Compare(a.LastActivity) })
	return out, nil
}

func (s *Service) Project(ctx context.Context, uid, name string) (ProjectDetail, error) {
	u, err := s.st.UserByUID(ctx, uid)
	if err != nil {
		return ProjectDetail{}, err
	}
	p, err := s.st.ProjectByFolder(ctx, uid, name)
	if errors.Is(err, store.ErrNotFound) {
		return ProjectDetail{}, ErrNotFound
	} else if err != nil {
		return ProjectDetail{}, err
	}
	cost, err := s.costByProject(ctx, uid, s.now().Add(-30*24*time.Hour))
	if err != nil {
		return ProjectDetail{}, err
	}
	active, _ := s.Active(ctx, uid)
	d, err := s.project(ctx, p, u.Slug, active, cost)
	dir := s.projectDir(p.Folder)
	d.SizeBytes = s.sizes(ctx, uid, []string{dir})[dir]
	return d, err
}

// ProjectExists checks projects and the trash.
func (s *Service) ProjectExists(ctx context.Context, uid, name string) (bool, error) {
	return s.st.NameTaken(ctx, uid, name)
}

// SuggestName asks the fast model for a folder name (metered, 5 s timeout).
func (s *Service) SuggestName(ctx context.Context, uid, description string) (string, error) {
	if s.Suggest == nil {
		return "", errors.New("suggest unavailable")
	}
	u, err := s.st.UserByUID(ctx, uid)
	if err != nil {
		return "", err
	}
	return s.Suggest(ctx, u, description)
}

// EnvStatus adds the disk usage in every state; 0/0 means unknown.
func (s *Service) EnvStatus(ctx context.Context, uid string) (EnvStatus, error) {
	st, err := s.envState(ctx, uid)
	st.DiskUsed, st.DiskQuota = s.disk(ctx, uid)
	return st, err
}

// disk asks oc-provision for the quota; a failure is logged and never fails the page.
func (s *Service) disk(ctx context.Context, uid string) (used, limit int64) {
	if s.DiskUsage == nil {
		return 0, 0
	}
	u, err := s.st.UserByUID(ctx, uid)
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		used, limit, err = s.DiskUsage(ctx, u.Slot)
	}
	if err != nil {
		s.log.WarnContext(ctx, "disk usage unavailable", "err", err)
		return 0, 0
	}
	return used, limit
}

func (s *Service) envState(ctx context.Context, uid string) (EnvStatus, error) {
	st := s.Status(uid)
	if st.State != StateAbsent {
		active, err := s.Active(ctx, uid)
		return EnvStatus{State: EnvRunning, Since: st.StartedAt, ActiveRuns: active}, err
	}
	starting, since, errID, full := s.StartState(uid)
	switch {
	case starting:
		return EnvStatus{State: EnvStarting, Since: since}, nil
	case full:
		return EnvStatus{State: EnvFull, Since: since}, nil
	case errID != "":
		return EnvStatus{State: EnvError, Since: since, ErrorID: errID}, nil
	}
	return EnvStatus{State: EnvStopped}, nil
}

func (s *Service) EnvStart(ctx context.Context, uid string) error { s.StartAsync(ctx, uid); return nil }
func (s *Service) EnvStop(ctx context.Context, uid string) error  { return s.Stop(ctx, uid) }
func (s *Service) EnvRestart(ctx context.Context, uid string) error {
	_, err := s.Restart(ctx, uid)
	return err
}
func (s *Service) EnvReset(ctx context.Context, uid string) error {
	_, err := s.Reset(ctx, uid)
	return err
}

// ListTrash lists trash items; Chats = root sessions stored at MoveToTrash.
func (s *Service) ListTrash(ctx context.Context, uid string) ([]TrashItem, error) {
	ts, err := s.st.Trash(ctx, uid)
	dirs := make([]string, len(ts))
	for i, t := range ts {
		dirs[i] = s.trashDir(t.ID)
	}
	size := s.sizes(ctx, uid, dirs)
	out := make([]TrashItem, 0, len(ts))
	for i, t := range ts {
		out = append(out, TrashItem{ID: t.ID, Name: t.Project.Folder, DeletedAt: t.DeletedAt, PurgeAt: t.PurgeAfter,
			SizeBytes: size[dirs[i]], Chats: t.Chats, Auto: t.Auto})
	}
	return out, err
}

// Usage sums usage per project since t. Rows are keyed by project id, as a
// trashed and a live project can share a folder name. Events without a
// project (name suggestions) or of purged projects land in Project "".
func (s *Service) Usage(ctx context.Context, uid string, since time.Time) ([]UsageRow, error) {
	rows, err := s.st.Usage(ctx, uid, since)
	if err != nil {
		return nil, err
	}
	known := map[string]UsageRow{}
	ps, err := s.st.Projects(ctx, uid)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		known[p.ID] = UsageRow{Project: p.Folder}
	}
	if ts, err := s.st.Trash(ctx, uid); err == nil {
		for _, t := range ts {
			known[t.Project.ID] = UsageRow{Project: t.Project.Folder, Trashed: true}
		}
	}
	idx := map[string]int{} // index, not pointer: append reallocates out
	var out []UsageRow
	for _, r := range rows {
		id := r.ProjectID
		if _, ok := known[id]; !ok {
			id = ""
		}
		i, ok := idx[id]
		if !ok {
			i = len(out)
			idx[id] = i
			out = append(out, known[id])
		}
		out[i].CostEUR += r.CostEUR
		out[i].CO2MinG += r.CO2LoG
		out[i].CO2MaxG += r.CO2HiG
		out[i].Chats += r.Sessions
	}
	return out, nil
}
