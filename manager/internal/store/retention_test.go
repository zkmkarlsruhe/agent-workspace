// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func folders(ps []Project) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Folder)
	}
	return out
}

// IdleProjects takes the newest of creation, touch and metered LLM call.
func TestIdleProjects(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "a1"})
	s.EnsureUser(ctx, identity.Identity{UID: uid(2), Username: "a2"})
	t0 := time.UnixMilli(1_700_000_000_000)
	day := 24 * time.Hour
	for _, p := range []Project{
		{ID: "p1", UID: uid(1), Folder: "alt", CreatedAt: t0},
		{ID: "p2", UID: uid(1), Folder: "beruehrt", CreatedAt: t0},
		{ID: "p3", UID: uid(1), Folder: "chat", CreatedAt: t0},
		{ID: "p4", UID: uid(2), Folder: "neu", CreatedAt: t0.Add(10 * day)},
	} {
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	s.TouchProject(ctx, uid(1), "beruehrt", t0.Add(10*day))
	s.TouchProject(ctx, uid(1), "beruehrt", t0.Add(day)) // older: no effect
	s.InsertUsage(ctx, UsageEvent{EventID: "e1", Start: t0.Add(10 * day), End: t0.Add(10 * day), UID: uid(1), ProjectID: "p3",
		Capability: "llm", Status: 200})

	cut := t0.Add(10 * day)
	idle, err := s.IdleProjects(ctx, cut)
	if err != nil {
		t.Fatal(err)
	}
	if got := folders(idle); len(got) != 1 || got[0] != "alt" {
		t.Fatalf("idle before cut: %v", got) // activity exactly at the cut is not idle
	}
	if idle, _ = s.IdleProjects(ctx, cut.Add(time.Millisecond)); len(idle) != 4 {
		t.Fatalf("idle after cut: %v", folders(idle))
	}
	p, _ := s.ProjectByFolder(ctx, uid(1), "chat")
	if !p.LastActivity.Equal(cut) {
		t.Fatalf("last activity from usage: %v", p.LastActivity)
	}

	// restore counts as activity: a restored project is not idle again at once
	alt, _ := s.ProjectByFolder(ctx, uid(1), "alt")
	s.MoveToTrash(ctx, alt, "t1", t0.Add(40*day), t0.Add(70*day), nil, true, true)
	tr, err := s.TrashItem(ctx, uid(1), "t1")
	if err != nil || !tr.Auto || !tr.Pending {
		t.Fatalf("%+v %v", tr, err)
	}
	if err := s.FinishPending(ctx, "t1", []TrashSession{{"ses_a", true}}); err != nil {
		t.Fatal(err)
	}
	if tr, _ = s.TrashItem(ctx, tr.Project.UID, "t1"); tr.Pending || tr.Chats != 1 {
		t.Fatalf("after finish: %+v", tr)
	}
	if err := s.RestoreTrash(ctx, tr, tr.Project.Folder, t0.Add(41*day)); err != nil {
		t.Fatal(err)
	}
	if idle, _ = s.IdleProjects(ctx, t0.Add(41*day)); slices.Contains(folders(idle), "alt") {
		t.Fatalf("restored project idle: %v", folders(idle))
	}
}

func TestDeleteUser(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	a, _ := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "anna"})
	s.EnsureUser(ctx, identity.Identity{UID: uid(2), Username: "bert"})
	now := time.Now()
	eur := 0.5
	for _, p := range []Project{{ID: "p1", UID: uid(1), Folder: "a"}, {ID: "p2", UID: uid(1), Folder: "b"}, {ID: "p3", UID: uid(2), Folder: "a"}} {
		s.CreateProject(ctx, p)
	}
	s.MoveToTrash(ctx, Project{ID: "p2", UID: uid(1), Folder: "b"}, "t1", now, now, []TrashSession{{"ses_1", true}}, false, false)
	for i, u := range []string{uid(1), uid(1), uid(2)} {
		s.InsertUsage(ctx, UsageEvent{EventID: string(rune('a' + i)), Start: now, End: now, UID: u, Capability: "llm", Status: 200, CostEUR: &eur})
	}
	d, err := s.DeleteUser(ctx, uid(1), "geloescht-x")
	if err != nil {
		t.Fatal(err)
	}
	if d != (Deleted{Projects: 1, Trash: 1, UsageEvents: 2}) {
		t.Fatalf("%+v", d)
	}
	if _, err := s.UserByUID(ctx, uid(1)); err != ErrNotFound {
		t.Fatal("user row left")
	}
	if ss, _ := s.TrashSessions(ctx, "t1"); len(ss) != 0 {
		t.Fatal("trash sessions left")
	}
	if tot, _ := s.UsageTotal(ctx, "geloescht-x", time.Time{}); tot.CostEUR != 1 {
		t.Fatalf("anonymised cost %v", tot.CostEUR)
	}
	if ps, _ := s.Projects(ctx, uid(2)); len(ps) != 1 {
		t.Fatal("other person's project removed")
	}
	// the same person comes back: fresh account, same slug and slot are free again
	b, err := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "anna"})
	if err != nil || b.Slot != a.Slot || b.Slug != a.Slug || b.Generation != 0 {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestLastActivityIgnoresForeignUID(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for i, name := range []string{"anna", "bert"} {
		if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(i + 1), Username: name}); err != nil {
			t.Fatal(err)
		}
	}
	created := time.Unix(1700000000, 0)
	if err := s.CreateProject(ctx, Project{ID: "p1", UID: uid(1), Folder: "old", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	for i, u := range []string{uid(1), uid(2)} {
		at := created.Add(time.Duration(i+1) * time.Hour)
		if err := s.InsertUsage(ctx, UsageEvent{EventID: u, UID: u, ProjectID: "p1", Start: at, End: at, Capability: "llm", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.ProjectByFolder(ctx, uid(1), "old")
	if err != nil || !p.LastActivity.Equal(created.Add(time.Hour)) {
		t.Fatalf("activity=%v err=%v", p.LastActivity, err)
	}
	idle, err := s.IdleProjects(ctx, created.Add(90*time.Minute))
	if err != nil || len(idle) != 1 {
		t.Fatalf("idle=%v err=%v", idle, err)
	}
}
