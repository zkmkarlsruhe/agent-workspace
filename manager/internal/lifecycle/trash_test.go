// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func sessionIDs(ss []ocapi.Session) []string {
	var ids []string
	for _, se := range ss {
		ids = append(ids, se.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestTrashPurgeDeletesStoredSessions(t *testing.T) {
	e := setupInMemory(t)
	ctx := context.Background()
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := e.s.CreateSession(ctx, uid1, "a", ""); err != nil {
			t.Fatal(err)
		}
	}
	dirA := e.s.projectDir("a")
	e.oc.Sessions = append(e.oc.Sessions,
		ocapi.Session{ID: "ses_child", ParentID: "ses_1", Location: ocapi.Location{Directory: dirA}},
		ocapi.Session{ID: "ses_other", Location: ocapi.Location{Directory: e.s.projectDir("b")}})
	p, _ := e.st.ProjectByFolder(ctx, uid1, "a")
	if err := e.st.InsertUsage(ctx, store.UsageEvent{EventID: "u1", Start: time.Now(), End: time.Now(), UID: uid1,
		ProjectID: p.ID, SessionID: "ses_metered1", Capability: "llm", Status: 200}); err != nil {
		t.Fatal(err)
	}

	if err := e.s.MoveToTrash(ctx, uid1, "a"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.oc.Evicted, dirA) || len(e.oc.Deleted) != 0 {
		t.Fatalf("trash must evict services but retain chats: %v %v", e.oc.Evicted, e.oc.Deleted)
	}
	tr, _ := e.s.ListTrash(ctx, uid1)
	if len(tr) != 1 || tr[0].Chats != 2 {
		t.Fatalf("trash %+v", tr)
	}
	stored, _ := e.st.TrashSessions(ctx, tr[0].ID)
	want := []store.TrashSession{{ID: "ses_1", Root: true}, {ID: "ses_2", Root: true}, {ID: "ses_child"}, {ID: "ses_metered1"}}
	if !slices.Equal(stored, want) {
		t.Fatalf("stored %+v", stored)
	}

	// a chat that appears under the old path later (e.g. a new project of the
	// same name) is not part of the trash item and must survive the purge
	e.oc.Sessions = append(e.oc.Sessions, ocapi.Session{ID: "ses_new", Location: ocapi.Location{Directory: dirA}})

	e.oc.FailDelete = "ses_2"
	if err := e.s.Purge(ctx, uid1, tr[0].ID); err != nil {
		t.Fatal("session delete failure must not leave a trash row without its folder", err)
	}
	if !slices.Equal(e.oc.Deleted, []string{"ses_1", "ses_2", "ses_child", "ses_metered1"}) {
		t.Fatalf("deleted %v", e.oc.Deleted)
	}
	if got := sessionIDs(e.oc.Sessions); !slices.Equal(got, []string{"ses_2", "ses_new", "ses_other"}) {
		t.Fatalf("left %v", got)
	}
	if tr, _ := e.st.Trash(ctx, uid1); len(tr) != 0 {
		t.Fatal("trash row must go")
	}
	if ss, _ := e.st.TrashSessions(ctx, tr[0].ID); len(ss) != 0 {
		t.Fatal("trash_sessions must cascade")
	}
	if _, err := os.Stat(e.s.trashDir(tr[0].ID)); !os.IsNotExist(err) {
		t.Fatal("folder must be gone")
	}
}

func TestRestoreMovesSessions(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, n := range []string{"r", "q"} {
		if err := e.s.CreateProject(ctx, uid1, NewProject{Name: n}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.CreateSession(ctx, uid1, n, ""); err != nil {
			t.Fatal(err)
		}
		if err := e.s.MoveToTrash(ctx, uid1, n); err != nil {
			t.Fatal(err)
		}
	}
	tr, _ := e.st.Trash(ctx, uid1) // newest first: q, r
	if err := e.s.Restore(ctx, uid1, tr[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if len(e.oc.Moved) != 0 {
		t.Fatalf("restore under the old name must not move: %v", e.oc.Moved)
	}
	if err := e.s.Restore(ctx, uid1, tr[1].ID, "r2"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.oc.Moved, []string{"ses_1=" + e.s.projectDir("r2")}) {
		t.Fatalf("moved %v", e.oc.Moved)
	}
	if ss, _ := e.s.Sessions(ctx, uid1, "r2"); len(ss) != 1 {
		t.Fatal("chat must list under the new name")
	}
}

func TestReapPurgesExpired(t *testing.T) {
	e := setup(t)
	e.s.cfg.MaxLifetime = 1000 * 24 * time.Hour // the container lives through the clock jump
	ctx := context.Background()
	clock := time.Now()
	e.s.now = func() time.Time { return clock }
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "alt"}); err != nil {
		t.Fatal(err)
	}
	e.s.CreateSession(ctx, uid1, "alt", "")
	if err := e.s.MoveToTrash(ctx, uid1, "alt"); err != nil {
		t.Fatal(err)
	}
	e.s.Reap(ctx)
	if tr, _ := e.st.Trash(ctx, uid1); len(tr) != 1 {
		t.Fatal("fresh item must stay")
	}
	clock = clock.Add(config.TrashRetention + time.Minute)
	e.s.Reap(ctx)
	if tr, _ := e.st.Trash(ctx, uid1); len(tr) != 0 || len(e.oc.Sessions) != 0 {
		t.Fatalf("expired item must be purged: %d sessions left", len(e.oc.Sessions))
	}
}

func TestSessionListAllPages(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.s.EnsureRunning(ctx, uid1)
	for i := range 205 {
		e.oc.Sessions = append(e.oc.Sessions, ocapi.Session{ID: fmt.Sprintf("ses_%03d", i), Location: ocapi.Location{Directory: "/workspace/p"}})
	}
	c, _ := e.s.Client(uid1)
	ss, err := c.SessionListAll(ctx, "/workspace/p")
	if err != nil || len(ss) != 205 || e.oc.Lists != 2 {
		t.Fatalf("%d sessions, %d requests, %v", len(ss), e.oc.Lists, err)
	}
}

func TestRestoreAndPurgeHoldPersonLock(t *testing.T) {
	for _, op := range []string{"restore", "purge"} {
		t.Run(op, func(t *testing.T) {
			e := setupInMemory(t)
			ctx := context.Background()
			if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "project"}); err != nil {
				t.Fatal(err)
			}
			if err := e.s.MoveToTrash(ctx, uid1, "project"); err != nil {
				t.Fatal(err)
			}
			trash, err := e.st.Trash(ctx, uid1)
			if err != nil || len(trash) != 1 {
				t.Fatal(trash, err)
			}
			checks := 0
			e.rt.AfterInspect = func(string) {
				checks++
				lock := e.s.lock(uid1)
				if lock.TryLock() {
					lock.Unlock()
					t.Error("filesystem operation ran without the person lock")
				}
			}
			if op == "restore" {
				err = e.s.Restore(ctx, uid1, trash[0].ID, "")
			} else {
				err = e.s.Purge(ctx, uid1, trash[0].ID)
			}
			if err != nil || checks < 2 {
				t.Fatalf("operation: %v, inspect checks %d", err, checks)
			}
		})
	}
}
