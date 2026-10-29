// SPDX-License-Identifier: MIT

package portal

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

// fake is an in-memory Backend for handler tests.
type fake struct {
	watchdog      []WatchdogNotice
	watchdogUID   string
	watchdogStops []string

	mu        sync.Mutex
	projects  map[string]*ProjectDetail
	files     map[string][]byte // "<project>/<path>"
	trash     []TrashItem
	env       EnvStatus
	suggest   string
	suggErr   error
	busy      map[string]bool
	usage     []UsageRow
	tree      []File
	truncated bool
	envErr    error // returned by EnvStart/Stop/Restart/Reset

	exportErr error // returned by Export before writing
	deleted   int   // DeleteAccount calls
	deleteErr error

	created  []NewProject
	sessions []string // "<project>|<prompt>"
	calls    []string
}

func newFake() *fake {
	return &fake{
		projects: map[string]*ProjectDetail{},
		files:    map[string][]byte{},
		busy:     map[string]bool{},
		env:      EnvStatus{State: EnvRunning},
	}
}

func (f *fake) addProject(name, desc string) {
	f.projects[name] = &ProjectDetail{Project: Project{Name: name, Description: desc, Created: time.Now()}}
}

func (f *fake) call(c string) { f.calls = append(f.calls, c) }

func (f *fake) ListProjects(ctx context.Context, uid string) ([]Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ps []Project
	for _, p := range f.projects {
		ps = append(ps, p.Project)
	}
	return ps, nil
}

func (f *fake) Project(ctx context.Context, uid, name string) (ProjectDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.projects[name]
	if !ok {
		return ProjectDetail{}, ErrNotFound
	}
	return *p, nil
}

func (f *fake) ProjectExists(ctx context.Context, uid, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.projects[name]; ok {
		return true, nil
	}
	for _, t := range f.trash {
		if t.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (f *fake) SuggestName(ctx context.Context, uid, d string) (string, error) {
	return f.suggest, f.suggErr
}

func (f *fake) CreateProject(ctx context.Context, uid string, p NewProject) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.projects[p.Name]; ok {
		return ErrExists
	}
	f.created = append(f.created, p)
	f.projects[p.Name] = &ProjectDetail{Project: Project{Name: p.Name, Description: p.Description}}
	return nil
}

func (f *fake) EnvStatus(ctx context.Context, uid string) (EnvStatus, error) { return f.env, nil }
func (f *fake) EnvStart(ctx context.Context, uid string) error {
	f.call("start")
	if f.envErr != nil {
		return f.envErr
	}
	f.env.State = EnvStarting
	return nil
}
func (f *fake) EnvStop(ctx context.Context, uid string) error    { f.call("stop"); return f.envErr }
func (f *fake) EnvRestart(ctx context.Context, uid string) error { f.call("restart"); return f.envErr }
func (f *fake) EnvReset(ctx context.Context, uid string) error   { f.call("reset"); return f.envErr }

func (f *fake) CreateSession(ctx context.Context, uid, project, prompt string) (string, error) {
	if f.env.State != EnvRunning {
		return "", ErrNotRunning
	}
	f.sessions = append(f.sessions, project+"|"+prompt)
	return fmt.Sprintf("https://anna.apps.example.org/server/xyz/session/ses_%d", len(f.sessions)), nil
}

func (f *fake) Tree(ctx context.Context, uid, project string) ([]File, bool, error) {
	return f.tree, f.truncated, nil
}

func (f *fake) OpenFile(ctx context.Context, uid, project, path string) (io.ReadCloser, File, error) {
	b, ok := f.files[project+"/"+path]
	if !ok {
		return nil, File{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), File{Name: path, Size: int64(len(b))}, nil
}

func (f *fake) WriteZip(ctx context.Context, uid, project, part string, w io.Writer) error {
	zw := zip.NewWriter(w)
	fw, _ := zw.Create(part + "/x.txt")
	fw.Write([]byte("x"))
	return zw.Close()
}

func (f *fake) Upload(ctx context.Context, uid, project, filename string, r io.Reader) (string, error) {
	if _, ok := f.projects[project]; !ok {
		return "", ErrNotFound
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	f.files[project+"/inbox/"+filename] = b
	return filename, nil
}

func (f *fake) MoveToTrash(ctx context.Context, uid, project string) error {
	if f.busy[project] {
		return ErrBusy
	}
	p, ok := f.projects[project]
	if !ok {
		return ErrNotFound
	}
	delete(f.projects, project)
	f.trash = append(f.trash, TrashItem{ID: project + "--1", Name: p.Name})
	return nil
}

func (f *fake) RenameProject(ctx context.Context, uid, project, newName string) error {
	if f.busy[project] {
		return ErrBusy
	}
	p, ok := f.projects[project]
	if !ok {
		return ErrNotFound
	}
	if taken, _ := f.ProjectExists(ctx, uid, newName); taken {
		return ErrExists
	}
	delete(f.projects, project)
	p.Name = newName
	f.projects[newName] = p
	return nil
}

func (f *fake) ListTrash(ctx context.Context, uid string) ([]TrashItem, error) { return f.trash, nil }

func (f *fake) Restore(ctx context.Context, uid, id, newName string) error {
	for i, t := range f.trash {
		if t.ID == id {
			n := t.Name
			if newName != "" {
				n = newName
			}
			if _, ok := f.projects[n]; ok {
				return ErrExists
			}
			f.addProject(n, "")
			f.trash = append(f.trash[:i], f.trash[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (f *fake) Purge(ctx context.Context, uid, id string) error {
	for i, t := range f.trash {
		if t.ID == id {
			f.trash = append(f.trash[:i], f.trash[i+1:]...)
			f.call("purge " + id)
			return nil
		}
	}
	return errors.New("purge: unknown id")
}

func (f *fake) Export(ctx context.Context, uid, project string, w io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exportErr != nil {
		return f.exportErr
	}
	if _, ok := f.projects[project]; project != "" && !ok {
		return ErrNotFound
	}
	f.call("export:" + project)
	zw := zip.NewWriter(w)
	fw, _ := zw.Create("README.txt")
	io.WriteString(fw, "export "+project)
	return zw.Close()
}

func (f *fake) DeleteAccount(ctx context.Context, uid string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted++
	return "anna", f.deleteErr
}

func (f *fake) Usage(ctx context.Context, uid string, since time.Time) ([]UsageRow, error) {
	return f.usage, nil
}

func (f *fake) Watchdog(_ context.Context, uid, project string) ([]WatchdogNotice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watchdogUID = uid
	if project != "" && f.projects[project] == nil {
		return nil, ErrNotFound
	}
	return append([]WatchdogNotice(nil), f.watchdog...), nil
}

func (f *fake) StopSilentSession(_ context.Context, uid, project, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watchdogUID = uid
	for i, n := range f.watchdog {
		if n.SessionID == session && n.SilentSeconds > 0 {
			f.watchdogStops = append(f.watchdogStops, session)
			f.watchdog = append(f.watchdog[:i], f.watchdog[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (f *fake) PrepareAssistant(ctx context.Context, uid, initPrompt, question, projectMode string) (string, string, error) {
	f.call("prepare-profile")
	return f.suggest, assistantPrompt(initPrompt, question), f.suggErr
}
func (f *fake) CreateAssistantProject(ctx context.Context, uid string, groups []string, p NewProject, assistant, hash string) error {
	def, _ := instance.Current().Assistant(assistant)
	if old := f.projects[p.Name]; old != nil && def.ProjectMode == "shared" {
		if old.Assistant != instance.Current().AssistantPin(def) {
			return ErrExists
		}
		return nil
	}
	if err := f.CreateProject(ctx, uid, p); err != nil {
		return err
	}
	f.projects[p.Name].Assistant = instance.Current().AssistantPin(def)
	f.env.State = EnvRunning
	return nil
}

func (f *fake) RestoreSession(_ context.Context, uid, project, session string) error {
	f.calls = append(f.calls, "restore-session:"+uid+":"+project+":"+session)
	p, ok := f.projects[project]
	if !ok {
		return ErrNotFound
	}
	for i, ch := range p.ArchivedChats {
		if ch.ID == session {
			ch.Archived = false
			p.ChatList = append(p.ChatList, ch)
			p.ArchivedChats = append(p.ArchivedChats[:i], p.ArchivedChats[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}
