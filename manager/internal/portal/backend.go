// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"errors"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"io"
	"time"
)

// Backend is what the portal needs from the manager core. Every call is scoped
// to one person (uid = X-Authentik-Uid, 64 hex). Project names reaching the
// backend are already validated against nameRE; file paths are relative,
// cleaned and free of ".." and dot segments. The backend must still resolve
// them inside the project folder and refuse symlinks (D33, docs/adr/README.md §3.3).
type Backend interface {
	RestoreSession(ctx context.Context, uid, project, session string) error
	Watchdog(ctx context.Context, uid, project string) ([]WatchdogNotice, error)
	StopSilentSession(ctx context.Context, uid, project, session string) error
	ListProjects(ctx context.Context, uid string) ([]Project, error)
	// Project returns ErrNotFound if the folder does not exist.
	Project(ctx context.Context, uid, name string) (ProjectDetail, error)
	// ProjectExists checks workspace/ and the trash (docs/adr/README.md §3.6 step 4).
	ProjectExists(ctx context.Context, uid, name string) (bool, error)
	// SuggestName asks the fast model (small, 5 s timeout) for a
	// folder name. The raw answer is fine: the portal normalises it and falls
	// back to a date name on error.
	SuggestName(ctx context.Context, uid, description string) (string, error)
	PrepareAssistant(ctx context.Context, uid, initPrompt, question, projectMode string) (name, prompt string, err error)
	CreateAssistantProject(ctx context.Context, uid string, groups []string, p NewProject, assistant, hash string) error
	// CreateProject creates inbox/, results/, work/, .project.json,
	// .project/brief.md, runs git init and writes .git/opencode. Returns
	// ErrExists if the name is taken, ErrQuota if storage is full. Rolls back
	// the folder on failure.
	CreateProject(ctx context.Context, uid string, p NewProject) error

	EnvStatus(ctx context.Context, uid string) (EnvStatus, error)
	// EnvStart returns as soon as the start is triggered; progress is visible
	// through EnvStatus. The backend owns the 30 s start timeout and reports
	// EnvError after it.
	EnvStart(ctx context.Context, uid string) error
	EnvStop(ctx context.Context, uid string) error
	EnvRestart(ctx context.Context, uid string) error
	// EnvReset recreates the container from the current image. workspace/,
	// config/, data/ and state/ are never touched.
	EnvReset(ctx context.Context, uid string) error

	// CreateSession needs a running environment (ErrNotRunning otherwise),
	// creates a chat in /workspace/<project> (body location, K1), sends
	// prompt if not empty and returns the deep link into OpenCode.
	CreateSession(ctx context.Context, uid, project, prompt string) (deepLink string, err error)

	// Tree lists the whole project recursively: dirs and regular files with
	// Path relative to the project, parents before children. Symlinks and
	// anything starting with "." are left out. truncated is true when the
	// backend capped the list.
	Tree(ctx context.Context, uid, project string) (files []File, truncated bool, err error)
	// OpenFile opens a regular file; ErrNotFound for missing files, dirs, symlinks and dot paths.
	OpenFile(ctx context.Context, uid, project, path string) (io.ReadCloser, File, error)
	// WriteZip streams part "results" or "all" (without dot files and
	// dirs, without symlinks) as a ZIP archive into w.
	WriteZip(ctx context.Context, uid, project, part string, w io.Writer) error
	// Upload writes r into <project>/inbox/. filename is a sanitised base
	// name; on collision the backend picks "name (2).ext" and returns the name
	// it used. Partial files must be removed when r fails. ErrQuota if full.
	Upload(ctx context.Context, uid, project, filename string, r io.Reader) (string, error)

	// MoveToTrash returns ErrBusy while an agent works in the project.
	MoveToTrash(ctx context.Context, uid, project string) error
	// RenameProject renames the folder and moves its chats along. ErrNotFound,
	// ErrExists (name taken, trash counts), ErrBusy while an agent works in it.
	RenameProject(ctx context.Context, uid, project, newName string) error
	ListTrash(ctx context.Context, uid string) ([]TrashItem, error)
	// Restore puts a trash item back. newName "" keeps the old name;
	// ErrExists if the target name is taken.
	Restore(ctx context.Context, uid, id, newName string) error
	// Purge deletes a trash item for good (files now, chats when the
	// environment runs next).
	Purge(ctx context.Context, uid, id string) error

	// Usage returns per-project totals since the given time.
	Usage(ctx context.Context, uid string, since time.Time) ([]UsageRow, error)

	// Export streams a ZIP of one project (project != "") or of all
	// projects plus the trash: files without dot paths and symlinks, each
	// chat as Markdown, README.txt. Starts the environment if needed;
	// ErrNotFound for an unknown project. Nothing is written before the
	// first archive bytes, so an early error can still become a page.
	Export(ctx context.Context, uid, project string, w io.Writer) error
	// DeleteAccount deletes all data of the person for good: projects,
	// trash, chats, settings, environment, account rows. ErrUnavailable if
	// the host helper is missing. The next visit starts a fresh account.
	DeleteAccount(ctx context.Context, uid string) (slug string, err error)
}

var (
	ErrNotFound    = errors.New("not found")
	ErrExists      = errors.New("already exists")
	ErrQuota       = errors.New("storage quota exceeded")
	ErrBusy        = errors.New("agent is working in this project")
	ErrNotRunning  = errors.New("environment not running")
	ErrUnavailable = errors.New("not available on this installation")
	ErrFull        = errors.New("all environment places taken")
)

// Project status values, derived by the core from the chat cache.
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
	CO2MinG      float64 // last 30 days, estimate range in g
	CO2MaxG      float64
	Recent       []Chat    // newest chats first (project list only)
	TrashAt      time.Time // the inactivity rule moves the project to the trash then
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
	CostEUR  float64 // from the gateway, same source as Usage
	CO2MinG  float64
	CO2MaxG  float64
	Link     string // deep link into OpenCode
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
	Auto      bool // moved by the inactivity rule, not by the person
}

type UsageRow struct {
	Project string // "" = no project: name suggestions and purged projects
	Trashed bool   // the project is in the trash
	CostEUR float64
	CO2MinG float64
	CO2MaxG float64
	Chats   int // chats with usage in the period (not all chats of the project)
}

type EnvState string

const (
	EnvRunning  EnvState = "running"
	EnvStopped  EnvState = "stopped"
	EnvStarting EnvState = "starting"
	EnvError    EnvState = "error"
	EnvFull     EnvState = "full" // all places taken, nothing started
)

type EnvStatus struct {
	State      EnvState
	Since      time.Time
	DiskUsed   int64
	DiskQuota  int64
	ErrorID    string   // short id that is also in the manager log
	ActiveRuns []string // session ids of running root sessions
}
