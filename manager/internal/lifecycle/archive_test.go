// SPDX-License-Identifier: MIT

package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle/lctest"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// This fixture uses the real client and lctest handler without a listening socket.
func archiveEnv(t *testing.T) (env, *time.Time, *bytes.Buffer) {
	t.Helper()
	d := *instance.Current()
	d.Sessions.ArchiveAfterDays = 30
	t.Cleanup(instance.Select(&d))
	clock := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	u, err := st.EnsureUser(context.Background(), identity.Identity{UID: uid1, Username: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	rt := lctest.NewRuntime()
	oc := &fakeOC{Now: func() time.Time { return clock }}
	logs := &bytes.Buffer{}
	s := New(config.Config{IdleTimeout: time.Hour, MaxLifetime: 365 * 24 * time.Hour}, rt, st, slog.New(slog.NewJSONHandler(logs, nil)))
	s.http = &http.Client{Transport: assistantTransport{oc}}
	s.baseURL = func(int) string { return "http://opencode.test" }
	s.now = oc.Now
	s.set(uid1, &running{slot: u.Slot, started: clock, lastRequest: clock})
	for _, name := range []string{"demo", "assistant"} {
		p := store.Project{ID: name, UID: uid1, Folder: name, CreatedAt: clock}
		if name == "assistant" {
			p.Assistant = assistants.Pin{ID: "support", Version: 1, Hash: strings.Repeat("a", 64)}
		}
		if err := st.CreateProject(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	return env{s, rt, oc, st}, &clock, logs
}

func archiveSession(id, dir string, when time.Time) ocapi.Session {
	se := ocapi.Session{ID: id, Title: id, Location: ocapi.Location{Directory: dir}}
	se.Time.Created, se.Time.Updated = float64(when.UnixMilli()), float64(when.UnixMilli())
	return se
}

func TestArchiveReapAndRestore(t *testing.T) {
	e, clock, logs := archiveEnv(t)
	ctx := context.Background()
	cutoff := clock.Add(-30 * 24 * time.Hour)
	dir := e.s.projectDir("demo")
	old := archiveSession("old", dir, cutoff.Add(-time.Millisecond))
	boundary := archiveSession("boundary", dir, cutoff)
	child := old
	child.ID, child.ParentID = "child", "old"
	archived := old
	archived.ID = "archived"
	archived.Time.Archived = new(float64)
	unknown := ocapi.Session{ID: "unknown", Location: old.Location}
	assistant := archiveSession("assistant", e.s.projectDir("assistant"), cutoff.Add(-time.Hour))
	e.oc.Sessions = []ocapi.Session{old, boundary, child, archived, unknown, assistant}
	lastRequest := e.s.get(uid1).lastRequest
	e.s.Reap(ctx)
	if !slices.Equal(e.oc.ArchiveCalls, []string{"old=true"}) {
		t.Fatal(e.oc.ArchiveCalls)
	}
	if !e.s.get(uid1).lastRequest.Equal(lastRequest) {
		t.Fatal("maintenance extended idle timeout")
	}
	e.s.Reap(ctx)
	if len(e.oc.ArchiveCalls) != 1 {
		t.Fatal("repeat archive", e.oc.ArchiveCalls)
	}
	p, _ := e.st.ProjectByFolder(ctx, uid1, "demo")
	view, err := e.s.project(ctx, p, "archive", nil, nil)
	if err != nil || len(view.ArchivedChats) != 2 || len(view.ChatList) != 2 || view.Chats != 4 {
		t.Fatalf("view %+v: %v", view, err)
	}
	if view.LastActivity != p.LastActivity {
		t.Fatal("archive counted as project activity")
	}
	if err := e.s.RestoreSession(ctx, uid1, "demo", "old"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RestoreSession(ctx, uid1, "demo", "old"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.oc.ArchiveCalls, []string{"old=true", "old=false"}) {
		t.Fatal(e.oc.ArchiveCalls)
	}
	*clock = clock.Add(24 * time.Hour)
	e.s.Touch(uid1)
	e.s.Reap(ctx)
	if !slices.Equal(e.oc.ArchiveCalls, []string{"old=true", "old=false", "boundary=true"}) {
		t.Fatal("restored chat needs a full grace period", e.oc.ArchiveCalls)
	}
	if strings.Count(logs.String(), `"op":"session_archive"`) != 2 || strings.Count(logs.String(), `"op":"session_unarchive"`) != 1 {
		t.Fatal(logs.String())
	}
	if len(e.rt.Specs) != 0 {
		t.Fatal("archive started a container")
	}
}

func TestArchiveSkipsAndRetries(t *testing.T) {
	for _, reason := range []string{"off", "stopped", "draining", "lock", "busy-root", "busy-child", "active-down", "patch-fails"} {
		t.Run(reason, func(t *testing.T) {
			e, clock, logs := archiveEnv(t)
			ctx := context.Background()
			e.oc.Sessions = []ocapi.Session{archiveSession("old", e.s.projectDir("demo"), clock.Add(-31*24*time.Hour))}
			r := e.s.get(uid1)
			switch reason {
			case "off":
				d := *instance.Current()
				d.Sessions.ArchiveAfterDays = 0
				restore := instance.Select(&d)
				defer restore()
			case "stopped":
				e.s.set(uid1, nil)
			case "draining":
				r.draining = true
			case "lock":
				e.s.lock(uid1).Lock()
			case "busy-root":
				e.oc.Active = []string{"old"}
			case "busy-child":
				e.oc.Active = []string{"child"}
			case "active-down":
				e.oc.ActiveDown = true
			case "patch-fails":
				e.oc.FailArchive = "old"
			}
			e.s.archiveIfIdle(ctx, uid1)
			if reason == "lock" {
				e.s.lock(uid1).Unlock()
			}
			if reason != "patch-fails" && len(e.oc.ArchiveCalls) != 0 {
				t.Fatal(e.oc.ArchiveCalls)
			}
			if strings.Contains(logs.String(), `"op":"session_archive"`) || e.oc.Sessions[0].Time.Archived != nil {
				t.Fatal("skip logged success or archived")
			}
			if len(e.rt.Specs) != 0 || !r.lastArchiveScan.IsZero() {
				t.Fatal("skip started environment or suppressed retry")
			}
			if reason == "off" {
				return
			}
			e.s.set(uid1, r)
			r.draining = false
			e.oc.Active, e.oc.ActiveDown, e.oc.FailArchive = nil, false, ""
			e.s.archiveIfIdle(ctx, uid1)
			if e.oc.Sessions[0].Time.Archived == nil {
				t.Fatal("retry did not archive")
			}
		})
	}
}

func TestArchiveFreshChecks(t *testing.T) {
	for _, change := range []string{"busy", "updated", "moved", "child", "archived", "missing"} {
		t.Run(change, func(t *testing.T) {
			e, clock, _ := archiveEnv(t)
			e.oc.Sessions = []ocapi.Session{archiveSession("old", e.s.projectDir("demo"), clock.Add(-31*24*time.Hour))}
			e.s.http.Transport = assistantTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/api/session/old" {
					switch change {
					case "busy":
						e.oc.Active = []string{"child"}
					case "updated":
						e.oc.Sessions[0].Time.Updated = float64(clock.UnixMilli())
					case "moved":
						e.oc.Sessions[0].Location.Directory = "/other"
					case "child":
						e.oc.Sessions[0].ParentID = "parent"
					case "archived":
						e.oc.Sessions[0].Time.Archived = new(float64)
					case "missing":
						e.oc.Sessions = nil
					}
				}
				e.oc.ServeHTTP(w, r)
			})}
			e.s.archiveIfIdle(context.Background(), uid1)
			if len(e.oc.ArchiveCalls) != 0 {
				t.Fatal(e.oc.ArchiveCalls)
			}
		})
	}
}

func TestArchivePaginationAndRestoreOwnership(t *testing.T) {
	e, clock, _ := archiveEnv(t)
	ctx := context.Background()
	for i := range 201 {
		e.oc.Sessions = append(e.oc.Sessions, archiveSession(fmt.Sprint(i), e.s.projectDir("demo"), *clock))
	}
	e.oc.Sessions[200].Time.Updated = float64(clock.Add(-31 * 24 * time.Hour).UnixMilli())
	e.s.archiveIfIdle(ctx, uid1)
	if !slices.Equal(e.oc.ArchiveCalls, []string{"200=true"}) {
		t.Fatal(e.oc.ArchiveCalls)
	}
	p, _ := e.st.ProjectByFolder(ctx, uid1, "demo")
	view, err := e.s.project(ctx, p, "archive", nil, nil)
	if err != nil || len(view.ChatList) != 200 || len(view.ArchivedChats) != 1 {
		t.Fatalf("pagination: %+v %v", view, err)
	}
	for _, args := range [][3]string{{"another-owner", "demo", "200"}, {uid1, "missing", "200"}, {uid1, "assistant", "200"}, {uid1, "demo", "missing"}} {
		if err := e.s.RestoreSession(ctx, args[0], args[1], args[2]); err != ErrNotFound {
			t.Fatalf("%v: %v", args, err)
		}
	}
	e.oc.Sessions[200].ParentID = "root"
	if err := e.s.RestoreSession(ctx, uid1, "demo", "200"); err != ErrNotFound {
		t.Fatal(err)
	}
	e.oc.Sessions[200].ParentID = ""
	e.oc.FailArchive = "200"
	if err := e.s.RestoreSession(ctx, uid1, "demo", "200"); !ocapi.IsStatus(err, 500) {
		t.Fatal(err)
	}
	e.s.set(uid1, nil)
	if err := e.s.RestoreSession(ctx, uid1, "demo", "200"); err != ErrNotRunning {
		t.Fatal(err)
	}
}

func TestArchiveShortThreshold(t *testing.T) {
	e, clock, _ := archiveEnv(t)
	d := *instance.Current()
	d.Sessions.ArchiveAfterDays = 1.0 / 86400 // one second
	defer instance.Select(&d)()
	e.oc.Sessions = []ocapi.Session{archiveSession("short", e.s.projectDir("demo"), *clock)}
	e.s.Reap(context.Background())
	*clock = clock.Add(999 * time.Millisecond)
	e.s.Reap(context.Background())
	if len(e.oc.ArchiveCalls) != 0 {
		t.Fatal("archived before threshold")
	}
	*clock = clock.Add(2 * time.Millisecond)
	e.s.Reap(context.Background())
	if !slices.Equal(e.oc.ArchiveCalls, []string{"short=true"}) {
		t.Fatal(e.oc.ArchiveCalls)
	}
}

func TestArchivePartialProgressAdvancesScan(t *testing.T) {
	e, clock, _ := archiveEnv(t)
	dir := e.s.projectDir("demo")
	e.oc.Sessions = []ocapi.Session{
		archiveSession("first", dir, clock.Add(-31*24*time.Hour)),
		archiveSession("second", dir, clock.Add(-32*24*time.Hour)),
	}
	e.oc.FailArchive = "second"
	e.s.http.Transport = assistantTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) < 5*e.s.callTimeout {
			t.Error("archive sweep lacks six-call timeout")
		}
		e.oc.ServeHTTP(w, r)
	})}
	e.s.archiveIfIdle(context.Background(), uid1)
	if e.oc.Sessions[0].Time.Archived == nil || !e.s.get(uid1).lastArchiveScan.Equal(*clock) {
		t.Fatal("partial progress lost", e.oc.ArchiveCalls)
	}
	calls := len(e.oc.ArchiveCalls)
	e.s.archiveIfIdle(context.Background(), uid1)
	if len(e.oc.ArchiveCalls) != calls {
		t.Fatal("partial sweep retried on next tick")
	}
}
