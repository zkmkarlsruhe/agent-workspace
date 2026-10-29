// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func trashNames(t *testing.T, e env) []string {
	t.Helper()
	ts, err := e.st.Trash(context.Background(), uid1)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range ts {
		out = append(out, it.Project.Folder)
	}
	slices.Sort(out)
	return out
}

// Inactivity rule while stopped: the store moves at once, the folder on the next start.
func TestTrashIdleStopped(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	clock := time.Now()
	e.s.now = func() time.Time { return clock }
	for _, n := range []string{"alt", "grenze", "aktiv"} {
		if err := e.s.CreateProject(ctx, uid1, NewProject{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	se := ocapi.Session{ID: "ses_alt", Title: "Alt", Location: ocapi.Location{Directory: e.s.projectDir("alt")}}
	e.oc.Sessions = append(e.oc.Sessions, se)
	created := clock
	e.st.TouchProject(ctx, uid1, "grenze", created.Add(time.Hour))
	e.st.TouchProject(ctx, uid1, "aktiv", created.Add(10*24*time.Hour))
	if err := e.s.EnvStop(ctx, uid1); err != nil {
		t.Fatal(err)
	}

	// exactly 30 days after the last activity is not yet idle
	clock = created.Add(time.Hour + config.ProjectIdleTrash)
	e.s.TrashIdle(ctx)
	if got := trashNames(t, e); !slices.Equal(got, []string{"alt"}) {
		t.Fatalf("trash at the boundary: %v", got)
	}
	clock = clock.Add(time.Millisecond)
	e.s.TrashIdle(ctx)
	e.s.TrashIdle(ctx) // idempotent
	if got := trashNames(t, e); !slices.Equal(got, []string{"alt", "grenze"}) {
		t.Fatalf("trash after the boundary: %v", got)
	}
	if e.s.get(uid1) != nil || len(e.rt.Specs) != 1 {
		t.Fatal("the rule must not start the environment")
	}
	ts, _ := e.st.Trash(ctx, uid1)
	for _, it := range ts {
		if !it.Auto || !it.Pending || !it.PurgeAfter.Equal(it.DeletedAt.Add(config.TrashRetention)) {
			t.Fatalf("%+v", it)
		}
	}
	if _, err := os.Stat(e.s.projectDir("alt")); err != nil {
		t.Fatal("folder must stay until the environment runs")
	}
	if ok, _ := e.s.ProjectExists(ctx, uid1, "alt"); !ok {
		t.Fatal("pending name must stay taken")
	}

	// next start moves the folders and records the chats
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	ts, _ = e.st.Trash(ctx, uid1)
	for _, it := range ts {
		if it.Pending {
			t.Fatalf("still pending %+v", it)
		}
		if _, err := os.Stat(e.s.trashDir(it.ID)); err != nil {
			t.Fatalf("folder not in trash: %v", err)
		}
		if it.Project.Folder == "alt" && it.Chats != 1 {
			t.Fatalf("chats of alt %+v", it)
		}
	}
	if _, err := os.Stat(e.s.projectDir("alt")); !os.IsNotExist(err) {
		t.Fatal("folder still in workspace")
	}
	lt, _ := e.s.ListTrash(ctx, uid1)
	if len(lt) != 2 || !lt[0].Auto {
		t.Fatalf("%+v", lt)
	}

	// restore counts as activity: the next sweep keeps it
	if err := e.s.Restore(ctx, uid1, lt[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	e.s.TrashIdle(ctx)
	if got := trashNames(t, e); len(got) != 1 {
		t.Fatalf("restored project trashed again: %v", got)
	}
}

// While running, Reap sweeps once a day and moves the folder in the same tick.
func TestTrashIdleRunningReap(t *testing.T) {
	e := setup(t)
	e.s.cfg.MaxLifetime = 1000 * 24 * time.Hour // the container lives through the clock jumps
	ctx := context.Background()
	clock := time.Now()
	e.s.now = func() time.Time { return clock }
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "alt"}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(config.ProjectIdleTrash + time.Minute)
	e.s.Touch(uid1) // keep the container alive for this tick
	// a running agent in the folder delays the move, not the trash row
	e.oc.Sessions = append(e.oc.Sessions, ocapi.Session{ID: "ses_busy", Location: ocapi.Location{Directory: e.s.projectDir("alt")}})
	e.oc.Active = []string{"ses_busy"}
	e.s.Reap(ctx)
	ts, _ := e.st.Trash(ctx, uid1)
	if len(ts) != 1 || !ts[0].Pending {
		t.Fatalf("busy: %+v", ts)
	}
	if err := e.s.Restore(ctx, uid1, ts[0].ID, ""); err != ErrBusy {
		t.Fatalf("restore of a busy pending item: %v", err)
	}
	e.oc.Active = nil
	e.s.Reap(ctx)
	if ts, _ = e.st.Trash(ctx, uid1); ts[0].Pending {
		t.Fatal("not moved on the next tick")
	}
	if !slices.Contains(e.oc.Evicted, e.s.projectDir("alt")) {
		t.Fatalf("pending trash location not evicted: %v", e.oc.Evicted)
	}
	// a new idle project within 24 h of the last sweep waits for the next day
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "neu"}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(config.ProjectIdleTrash + time.Minute)
	e.s.lastSweep = clock.Add(-time.Hour)
	e.s.Touch(uid1)
	e.s.Reap(ctx)
	if _, err := e.st.ProjectByFolder(ctx, uid1, "neu"); err != nil {
		t.Fatalf("swept twice a day: %v", err)
	}
	e.s.lastSweep = clock.Add(-sweepEvery)
	e.s.Reap(ctx)
	if _, err := e.st.ProjectByFolder(ctx, uid1, "neu"); err != store.ErrNotFound {
		t.Fatalf("daily sweep missing: %v", err)
	}
}

// Portal and chat activity keep a project out of the trash.
func TestActivityIsRecorded(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	clock := time.Now()
	e.s.now = func() time.Time { return clock }
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	last := func() time.Time {
		p, err := e.st.ProjectByFolder(ctx, uid1, "a")
		if err != nil {
			t.Fatal(err)
		}
		return p.LastActivity
	}
	step := func(what string, f func()) {
		t.Helper()
		clock = clock.Add(time.Hour)
		f()
		if got := last(); !got.Equal(time.UnixMilli(clock.UnixMilli())) {
			t.Fatalf("%s: last activity %v, want %v", what, got, clock)
		}
	}
	step("upload", func() { e.s.Upload(ctx, uid1, "a", "x.txt", strings.NewReader("x")) })
	step("chat", func() { e.s.CreateSession(ctx, uid1, "a", "") })
	step("zip", func() { e.s.WriteZip(ctx, uid1, "a", "all", io.Discard) })
	// a chat updated later (seen while running) counts too
	clock = clock.Add(time.Hour)
	se := &e.oc.Sessions[len(e.oc.Sessions)-1]
	se.Time.Updated = float64(clock.UnixMilli())
	if _, err := e.s.Project(ctx, uid1, "a"); err != nil {
		t.Fatal(err)
	}
	if got := last(); !got.Equal(time.UnixMilli(clock.UnixMilli())) {
		t.Fatalf("chat update: %v", got)
	}
	// metered LLM calls count without a touch
	clock = clock.Add(time.Hour)
	p, _ := e.st.ProjectByFolder(ctx, uid1, "a")
	e.st.InsertUsage(ctx, store.UsageEvent{EventID: "u", Start: clock, End: clock, UID: uid1, ProjectID: p.ID, Capability: "llm", Status: 200})
	if got := last(); !got.Equal(time.UnixMilli(clock.UnixMilli())) {
		t.Fatalf("usage: %v", got)
	}
}
