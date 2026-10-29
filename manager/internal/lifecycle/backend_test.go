// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestBackendViews(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if st, _ := e.s.EnvStatus(ctx, uid1); st.State != EnvStopped || st.DiskQuota != 0 {
		t.Fatal(st)
	}
	var diskSlot int
	e.s.DiskUsage = func(_ context.Context, slot int) (int64, int64, error) {
		diskSlot = slot
		return 7, 20, nil
	}
	if st, err := e.s.EnvStatus(ctx, uid1); err != nil || st.State != EnvStopped || st.DiskUsed != 7 || st.DiskQuota != 20 || diskSlot != 1 {
		t.Fatalf("stopped disk: %+v %v slot %d", st, err, diskSlot)
	}
	e.s.DiskUsage = func(context.Context, int) (int64, int64, error) { return 1, 2, errors.New("helper down") }
	if st, err := e.s.EnvStatus(ctx, uid1); err != nil || st.DiskUsed != 0 || st.DiskQuota != 0 {
		t.Fatalf("failing helper must show unknown: %+v %v", st, err)
	}
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "a", Description: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	pa, _ := e.st.ProjectByFolder(ctx, uid1, "a")
	cost, lo, hi := 0.5, 2.0, 8.0
	e.st.InsertUsage(ctx, store.UsageEvent{EventID: "1", Start: time.Now(), End: time.Now(), UID: uid1, ProjectID: pa.ID,
		SessionID: "ses_a1", Capability: "llm", Status: 200, CostEUR: &cost, CO2LoG: &lo, CO2HiG: &hi})

	se := ocapi.Session{ID: "ses_a1", Location: ocapi.Location{Directory: e.s.projectDir("a")}, Outcome: "succeeded", CostUSD: 1.1}
	se.Time.Updated = float64(time.Now().Add(time.Hour).UnixMilli())
	e.oc.Sessions = append(e.oc.Sessions, se, ocapi.Session{ID: "ses_sub", ParentID: "ses_a1", Location: se.Location})

	if err := os.WriteFile(filepath.Join(e.s.ws, "a", "work", "gross.bin"), make([]byte, 100_000), 0o644); err != nil {
		t.Fatal(err)
	}
	ps, err := e.s.ListProjects(ctx, uid1)
	if err != nil || len(ps) != 2 || ps[0].Name != "a" || ps[0].Chats != 1 || ps[0].Status != StatusDone || ps[0].CostEUR != 0.5 ||
		ps[0].CO2MinG != 2 || ps[0].CO2MaxG != 8 {
		t.Fatalf("%+v %v", ps, err)
	}
	if ps[0].SizeBytes < 100_000 || ps[1].SizeBytes <= 0 || ps[0].SizeBytes <= ps[1].SizeBytes {
		t.Fatalf("sizes a=%d b=%d", ps[0].SizeBytes, ps[1].SizeBytes)
	}
	e.oc.Active = []string{"ses_a1"}
	d, err := e.s.Project(ctx, uid1, "a")
	if err != nil || d.Status != StatusWorking || d.ChatList[0].Status != StatusWorking || d.ChatList[0].CostEUR != 0.5 ||
		d.ChatList[0].CO2MaxG != 8 || d.ChatList[0].Link == "" || d.SizeBytes != ps[0].SizeBytes {
		t.Fatalf("%+v %v", d, err)
	}
	e.oc.Forms = 1
	if d, _ := e.s.Project(ctx, uid1, "a"); d.Status != StatusWaiting {
		t.Fatal("pending form must show wartet")
	}
	if _, err := e.s.Project(ctx, uid1, "zzz"); err != ErrNotFound {
		t.Fatal(err)
	}
	if st, _ := e.s.EnvStatus(ctx, uid1); st.State != EnvRunning || len(st.ActiveRuns) != 1 {
		t.Fatalf("%+v", st)
	}
	if ok, _ := e.s.ProjectExists(ctx, uid1, "b"); !ok {
		t.Fatal("exists")
	}
	rows, err := e.s.Usage(ctx, uid1, time.Now().Add(-time.Hour))
	if err != nil || len(rows) != 1 || rows[0].Project != "a" || rows[0].CostEUR != 0.5 || rows[0].Chats != 1 ||
		rows[0].CO2MinG != 2 {
		t.Fatalf("%+v %v", rows, err)
	}
	e.oc.Active = nil
	if err := e.s.MoveToTrash(ctx, uid1, "b"); err != nil {
		t.Fatal(err)
	}
	if tr, _ := e.s.ListTrash(ctx, uid1); len(tr) != 1 || tr[0].Name != "b" || !tr[0].PurgeAt.After(tr[0].DeletedAt) || tr[0].SizeBytes != ps[1].SizeBytes {
		t.Fatalf("%+v", tr)
	}
	if ok, _ := e.s.ProjectExists(ctx, uid1, "b"); !ok {
		t.Fatal("trashed name must count as existing")
	}

	if err := e.s.EnvStop(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	// stopped: the trash does not start the container, so its sizes are unknown
	if tr, _ := e.s.ListTrash(ctx, uid1); tr[0].SizeBytes != 0 || e.s.get(uid1) != nil {
		t.Fatalf("stopped trash size %+v", tr)
	}
	// the project list never waits for a start: it triggers one and answers
	// without chats, last activity = the stored one (the chat update seen
	// while running); once running the chats are back
	chatAt := time.UnixMilli(int64(se.Time.Updated))
	ps, err = e.s.ListProjects(ctx, uid1)
	if err != nil || len(ps) != 1 || len(ps[0].Recent) != 0 || ps[0].Chats != 0 || ps[0].Status != StatusNone || !ps[0].LastActivity.Equal(chatAt) ||
		!ps[0].TrashAt.Equal(chatAt.Add(config.ProjectIdleTrash)) {
		t.Fatalf("list while stopped %+v %v", ps, err)
	}
	waitRunning(t, e.s)
	if ps, _ := e.s.ListProjects(ctx, uid1); len(ps[0].Recent) != 1 || ps[0].Recent[0].Link == "" {
		t.Fatalf("list lacks chats once running %+v", ps)
	}
	e.s.EnvStop(ctx, uid1)
	e.s.EnvStart(ctx, uid1)
	waitRunning(t, e.s)
}

func waitRunning(t *testing.T, s *Service) {
	t.Helper()
	for range 200 {
		if st, _ := s.EnvStatus(context.Background(), uid1); st.State == EnvRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("environment did not reach running")
}

// After a failed start the list does not retry, so the start page shows the error.
func TestListProjectsNoRetryAfterFailedStart(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.s.mu.Lock()
	e.s.starts[uid1] = &startState{done: true, err: errors.New("boom"), errID: "abc"}
	e.s.mu.Unlock()
	if _, err := e.s.ListProjects(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.s.EnvStatus(ctx, uid1); st.State != EnvError || len(e.rt.Specs) != 0 {
		t.Fatalf("list retried a failed start: %+v specs %d", st, len(e.rt.Specs))
	}
}

// Usage must keep every project's sums when the result slice grows (it used
// to keep pointers into a reallocated slice).
func TestUsageManyProjects(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	names := []string{"a", "b", "c", "d", "e"}
	for i, n := range names {
		if err := e.s.CreateProject(ctx, uid1, NewProject{Name: n}); err != nil {
			t.Fatal(err)
		}
		p, _ := e.st.ProjectByFolder(ctx, uid1, n)
		for j := 0; j <= i; j++ {
			c := 1.0
			e.st.InsertUsage(ctx, store.UsageEvent{EventID: fmt.Sprint(n, j), Start: time.Now(), End: time.Now(),
				UID: uid1, ProjectID: p.ID, SessionID: "ses_" + n, Capability: "llm", Status: 200, CostEUR: &c, CO2HiG: &c})
		}
	}
	rows, err := e.s.Usage(ctx, uid1, time.Now().Add(-time.Hour))
	if err != nil || len(rows) != len(names) {
		t.Fatalf("%+v %v", rows, err)
	}
	for _, r := range rows {
		want := float64(1 + slices.Index(names, r.Project))
		if r.CostEUR != want || r.CO2MaxG != want || r.Chats != 1 {
			t.Errorf("%+v, want cost %v", r, want)
		}
	}
}

// A trashed and a live project with the same folder name stay two rows; the
// trashed one is marked; unknown project ids join the row without project.
func TestUsageTrashedSameName(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.s.CreateProject(ctx, uid1, NewProject{Name: "a"})
	old, _ := e.st.ProjectByFolder(ctx, uid1, "a")
	if err := e.s.MoveToTrash(ctx, uid1, "a"); err != nil {
		t.Fatal(err)
	}
	// the portal refuses a trashed name; the store does not, so the rows must not merge by name
	live := store.Project{ID: "live-a", UID: uid1, Folder: "a", CreatedAt: time.Now()}
	if err := e.st.CreateProject(ctx, live); err != nil {
		t.Fatal(err)
	}
	for i, ev := range []struct {
		project string
		eur     float64
	}{{old.ID, 1}, {live.ID, 2}, {"", 4}, {"geloescht", 8}} {
		e.st.InsertUsage(ctx, store.UsageEvent{EventID: fmt.Sprint(i), Start: time.Now(), End: time.Now(), UID: uid1,
			ProjectID: ev.project, Capability: "llm", Status: 200, CostEUR: &ev.eur})
	}
	rows, err := e.s.Usage(ctx, uid1, time.Now().Add(-time.Hour))
	got := map[UsageRow]bool{}
	for _, r := range rows {
		got[r] = true
	}
	if err != nil || len(rows) != 3 || !got[UsageRow{Project: "a", Trashed: true, CostEUR: 1}] ||
		!got[UsageRow{Project: "a", CostEUR: 2}] || !got[UsageRow{CostEUR: 12}] {
		t.Fatalf("%+v %v", rows, err)
	}
}

// A session without time.updated (fresh chat) falls back to time.created,
// and without both to zero, never to 1970.
func TestSessionTime(t *testing.T) {
	var se ocapi.Session
	if !sessionTime(se).IsZero() {
		t.Error("no timestamps must be zero")
	}
	se.Time.Created = 1_700_000_000_000
	if sessionTime(se).UnixMilli() != 1_700_000_000_000 {
		t.Error("created fallback")
	}
	se.Time.Updated = 1_700_000_100_000
	if sessionTime(se).UnixMilli() != 1_700_000_100_000 {
		t.Error("updated first")
	}
}
