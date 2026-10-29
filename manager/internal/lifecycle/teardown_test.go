// SPDX-License-Identifier: MIT

package lifecycle

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

func (e env) setRunning(name string, running bool, labels map[string]string) {
	e.rt.Mu.Lock()
	defer e.rt.Mu.Unlock()
	e.rt.Ctrs[name].Running = running
	if labels != nil {
		e.rt.Ctrs[name].Labels = labels
	}
}

func (e env) gone(name string) bool {
	_, err := e.rt.Inspect(context.Background(), name)
	return err == runtime.ErrNotFound
}

// A Suspect that saw the stopped container before a restart replaced it must
// not remove the new container or forget it (its lease would leak).
func TestSuspectRacesRestart(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	old := e.s.get(uid1)
	e.setRunning("oc-user1", false, nil)
	restarted := make(chan error, 1)
	var fired atomic.Bool
	e.rt.Mu.Lock()
	e.rt.AfterInspect = func(string) {
		if fired.Swap(true) {
			return
		}
		go func() { _, err := e.s.Restart(ctx, uid1); restarted <- err }()
		select { // with the lock held by Suspect the restart has to wait
		case err := <-restarted:
			restarted <- err
		case <-time.After(300 * time.Millisecond):
		}
	}
	e.rt.Mu.Unlock()
	e.s.Suspect(uid1)
	if err := <-restarted; err != nil {
		t.Fatal(err)
	}
	r := e.s.get(uid1)
	c, err := e.rt.Inspect(ctx, "oc-user1")
	if r == nil || r == old || err != nil || !c.Running || c.Labels["oc.generation"] != strconv.FormatInt(r.gen, 10) {
		t.Fatalf("restarted container lost: tracked %+v, container %+v %v", r, c, err)
	}
	if _, release, ok := r.lease.bind(ctx); !ok {
		t.Fatal("lease of the new generation ended")
	} else {
		release()
	}
	if _, _, ok := old.lease.bind(ctx); ok {
		t.Fatal("lease of the old generation still open")
	}
}

// A stopped container in the slot that carries another person or generation
// is not this person's: Suspect forgets the stale entry but leaves it alone.
func TestSuspectLeavesForeignContainer(t *testing.T) {
	for _, labels := range []map[string]string{
		{"oc.uid": uid2, "oc.generation": "1"},
		{"oc.uid": uid1, "oc.generation": "99"},
	} {
		e := setup(t)
		if _, err := e.s.EnsureRunning(context.Background(), uid1); err != nil {
			t.Fatal(err)
		}
		e.setRunning("oc-user1", false, labels)
		e.s.Suspect(uid1)
		if e.gone("oc-user1") || e.s.get(uid1) != nil {
			t.Fatalf("%v: removed %v, still tracked %v", labels, e.gone("oc-user1"), e.s.get(uid1) != nil)
		}
	}
}

// The tool containers go with a main container that disappeared, whichever
// path notices it (Suspect after a dial error, EnsureRunning).
func TestToolsGoWithVanishedMain(t *testing.T) {
	for _, via := range []string{"suspect", "ensure"} {
		e := setup(t)
		ctx := context.Background()
		e.rt.Root = t.TempDir()
		if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.s.RunToolbox(ctx, uid1, "media", toolImg, "/workspace", "true", &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if via == "suspect" {
			e.rt.Remove(ctx, "oc-user1")
			e.s.Suspect(uid1)
		} else {
			e.setRunning("oc-user1", false, nil)
			if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
				t.Fatal(err)
			}
		}
		if !e.gone("oc-user1-tb-media") {
			t.Fatalf("%s: tool container survived its main", via)
		}
	}
}

// The reaper removes tool containers whose main container is not tracked
// (a teardown that failed, a manager that lost track) and keeps the others.
func TestReapSweepsOrphanTools(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.rt.Root = t.TempDir()
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.RunToolbox(ctx, uid1, "media", toolImg, "/workspace", "true", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// an orphan under the name and label before the English rename is swept too
	orphan := runtime.Spec{Name: "oc-user2-w-medien", Image: toolImg, Tool: true,
		Labels: map[string]string{runtime.LabelManaged: "1", "oc.uid": uid2, "oc.slot": "2", oldLabelToolbox: "medien"}}
	e.rt.Create(ctx, orphan)
	e.rt.Start(ctx, orphan.Name)
	e.s.Reap(ctx)
	if !e.gone(orphan.Name) || e.gone("oc-user1-tb-media") {
		t.Fatalf("sweep: orphan gone %v, live tool gone %v", e.gone(orphan.Name), e.gone("oc-user1-tb-media"))
	}
}

// A command that stops its own timeout supervisor (same uid) cannot outlive
// the deadline: the manager removes the tool container, which kills it, and
// the next run gets a fresh one.
func TestToolboxDeadlineRemovesContainer(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.rt.Root = t.TempDir()
	e.rt.KillOnRemove = true
	e.s.toolboxKill = 500 * time.Millisecond
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, _, err := e.s.RunToolbox(ctx, uid1, "media", toolImg, "/workspace", `kill -STOP "$PPID"; exec sleep 86400`, &bytes.Buffer{})
		done <- result{code, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run outlived its deadline")
	}
	if res.code != 124 || res.err != nil || !e.gone("oc-user1-tb-media") {
		t.Fatalf("deadline: code %d err %v, container gone %v", res.code, res.err, e.gone("oc-user1-tb-media"))
	}
	for i := 0; e.rt.Execs("oc-user1-tb-media") > 0; i++ {
		if i > 100 {
			t.Fatal("the stopped command still runs")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var out bytes.Buffer
	if code, _, err := e.s.RunToolbox(ctx, uid1, "media", toolImg, "/workspace", "echo wieder", &out); code != 0 || err != nil || out.String() != "wieder\n" {
		t.Fatalf("next run: %d %v %q", code, err, out.String())
	}
}

// Runs in one tool container take turns: a deadline removes the container
// under every run in it.
func TestToolboxRunsSerialized(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.rt.Root = t.TempDir()
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, _, err := e.s.RunToolbox(ctx, uid1, "media", toolImg, "/workspace", "echo s >> log; sleep 0.2; echo e >> log", &bytes.Buffer{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if b, _ := os.ReadFile(filepath.Join(e.rt.Root, "log")); string(b) != "s\ne\ns\ne\ns\ne\n" {
		t.Fatalf("runs overlapped: %q", b)
	}
}
