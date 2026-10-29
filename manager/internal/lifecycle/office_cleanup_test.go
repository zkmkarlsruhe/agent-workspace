// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type officeDrainRuntime struct {
	runtime.Runtime
	mu      sync.Mutex
	stopped map[string]bool
	entered chan string
	drain   <-chan struct{}
	t       *testing.T
}

func (r *officeDrainRuntime) Stop(ctx context.Context, id string, timeout time.Duration) error {
	if timeout != 16*time.Second {
		r.t.Errorf("stop timeout=%v", timeout)
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 17*time.Second {
		r.t.Error("unbounded stop request")
	}
	if r.entered != nil {
		r.entered <- id
	}
	if r.drain != nil {
		select {
		case <-r.drain:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.stopped[id] = true
	r.mu.Unlock()
	return r.Runtime.Stop(ctx, id, timeout)
}
func (r *officeDrainRuntime) Remove(ctx context.Context, id string) error {
	r.mu.Lock()
	stopped := r.stopped[id]
	r.mu.Unlock()
	if !stopped {
		r.t.Errorf("removed %s before graceful stop completed", id)
	}
	return r.Runtime.Remove(ctx, id)
}
func releaseDrain(drain chan struct{}) {
	select {
	case <-drain:
	default:
		close(drain)
	}
}
func awaitStops(t *testing.T, entered <-chan string) {
	t.Helper()
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("worker stops serialized behind drain")
		}
	}
}

func TestOfficeShutdownDrainsWorkersInParallelAndKeepsMainRuntime(t *testing.T) {
	f := newOfficeFixture(t)
	officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`)
	uid2 := strings.Repeat("b", 64)
	f.s.set(uid2, &running{slot: 2, gen: 1, lease: newLease(), started: time.Now()})
	officeCall(t, f.s, uid2, "office_open", `{"fileId":"2"}`)
	f.rt.Ctrs["oc-user1"] = &runtime.Container{ID: "main", Name: "oc-user1", Running: true}
	drain := make(chan struct{})
	defer releaseDrain(drain)
	probe := &officeDrainRuntime{Runtime: f.s.rt, stopped: map[string]bool{}, entered: make(chan string, 2), drain: drain, t: t}
	f.s.rt = probe
	f.entered = make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		_, err := f.s.CallOffice(context.Background(), uid1, "office_replace", json.RawMessage(`{"documentId":"1:1"}`))
		writeDone <- err
	}()
	<-f.entered
	done := make(chan struct{})
	go func() { f.s.Close(); close(done) }()
	awaitStops(t, probe.entered)
	select {
	case <-done:
		t.Fatal("shutdown returned before worker drain")
	default:
	}
	if len(f.rt.Removed) != 0 {
		t.Fatal("removed worker during drain")
	}
	close(drain)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown stuck")
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("in-flight Office call was not cancelled")
	}
	if f.writes.Load() != 1 {
		t.Fatal("in-flight write replayed")
	}
	if len(f.rt.Removed) != 2 || f.rt.Ctrs["oc-user1"] == nil {
		t.Fatalf("removed %v; main runtime must remain", f.rt.Removed)
	}
}
func TestOfficeOrphanCleanupStopsInParallel(t *testing.T) {
	for _, operation := range []string{"reconcile", "sweep"} {
		t.Run(operation, func(t *testing.T) {
			e := setup(t)
			for i, uid := range []string{uid1, strings.Repeat("b", 64)} {
				id := []string{"old-one", "old-two"}[i]
				e.rt.Ctrs[id] = &runtime.Container{ID: id, Name: "oc-user-office", Running: true, Labels: map[string]string{"oc.uid": uid, "oc.office": "nextcloud"}}
			}
			drain := make(chan struct{})
			defer releaseDrain(drain)
			probe := &officeDrainRuntime{Runtime: e.rt, stopped: map[string]bool{}, entered: make(chan string, 2), drain: drain, t: t}
			e.s.rt = probe
			done := make(chan error, 1)
			go func() {
				if operation == "reconcile" {
					done <- e.s.Reconcile(context.Background())
				} else {
					e.s.sweepOffice(context.Background())
					done <- nil
				}
			}()
			awaitStops(t, probe.entered)
			if len(e.rt.Removed) != 0 {
				t.Fatal("removed before drain")
			}
			close(drain)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("cleanup stuck")
			}
			if len(e.rt.Removed) != 2 {
				t.Fatalf("removed %v", e.rt.Removed)
			}
		})
	}
}
func TestOfficeReplacementStopsUntrackedWorker(t *testing.T) {
	f := newOfficeFixture(t)
	f.rt.Ctrs["oc-user1-office"] = &runtime.Container{ID: "old", Name: "oc-user1-office", Running: true, Labels: map[string]string{"oc.uid": uid1, "oc.office": "nextcloud"}}
	f.s.rt = &officeDrainRuntime{Runtime: f.s.rt, stopped: map[string]bool{}, t: t}
	officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`)
	if len(f.rt.Removed) != 1 || f.rt.Removed[0] != "oc-user1-office" {
		t.Fatalf("old worker not replaced: %v", f.rt.Removed)
	}
}

type failedOfficeStop struct {
	runtime.Runtime
	stopDeadline time.Time
	removed      bool
	t            *testing.T
}

func (r *failedOfficeStop) Stop(ctx context.Context, _ string, _ time.Duration) error {
	r.stopDeadline, _ = ctx.Deadline()
	return context.DeadlineExceeded
}
func (r *failedOfficeStop) Remove(ctx context.Context, _ string) error {
	deadline, _ := ctx.Deadline()
	if deadline.Sub(r.stopDeadline) < 7*time.Second {
		r.t.Error("no removal budget after stop timeout")
	}
	r.removed = true
	return errors.New("remove failed")
}
func TestOfficeStopFailureStillAttemptsRemovalWithReservedBudget(t *testing.T) {
	e := setup(t)
	probe := &failedOfficeStop{Runtime: e.rt, t: t}
	e.s.rt = probe
	if err := e.s.removeOffice(context.Background(), "worker"); err == nil || !errors.Is(err, context.DeadlineExceeded) || !probe.removed {
		t.Fatalf("error=%v removed=%v", err, probe.removed)
	}
}

type watchdogDrainTransport struct {
	entered   chan struct{}
	cancelled chan struct{}
	release   <-chan struct{}
}

func (tr watchdogDrainTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	close(tr.entered)
	<-r.Context().Done()
	close(tr.cancelled)
	<-tr.release
	return nil, r.Context().Err()
}
func TestLifecycleLoopJoinsWatchdogBeforeReturning(t *testing.T) {
	e := setup(t)
	if _, err := e.s.EnsureRunning(context.Background(), uid1); err != nil {
		t.Fatal(err)
	}
	e.s.cfg.ReapInterval = time.Hour
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer releaseDrain(release)
	e.s.http.Transport = watchdogDrainTransport{entered, cancelled, release}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { e.s.Loop(ctx); close(done) }()
	<-entered
	cancel()
	<-cancelled
	select {
	case <-done:
		t.Fatal("Loop returned before watchdog cancellation settled")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Loop did not join watchdog")
	}
}

func TestCancelledOfficeCallDoesNotStartAnotherCleanup(t *testing.T) {
	f := newOfficeFixture(t)
	officeCall(t, f.s, uid1, "office_open", `{"fileId":"1"}`)
	worker := f.s.officeSlot(uid1).worker
	f.s.OfficeCreds = func(context.Context, string) (string, string, error) {
		t.Fatal("cancelled queued call reached credentials")
		return "", "", context.Canceled
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.s.CallOffice(ctx, uid1, "office_read", json.RawMessage(`{"documentId":"1:1"}`)); err == nil {
		t.Fatal("cancelled call accepted")
	}
	if f.s.officeSlot(uid1).worker != worker || len(f.rt.Removed) != 0 {
		t.Fatal("cancelled request restarted cleanup")
	}
}
