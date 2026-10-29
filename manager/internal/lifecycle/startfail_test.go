// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

// A failed start or readiness probe is an error and leaves no container.
func TestStartFailureIsError(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.rt.StartErr = errors.New("boom")
	if _, err := e.s.EnsureRunning(ctx, uid1); err == nil || e.s.Status(uid1).State == StateReady {
		t.Fatalf("start failure reported as success: %v", err)
	}
	if len(e.rt.Ctrs) != 0 {
		t.Fatal("failed container not removed")
	}
	e.rt.StartErr = nil
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	e.s.baseURL = func(int) string { return srv.URL }
	if _, err := e.s.EnsureRunning(ctx, uid1); err == nil || e.s.Status(uid1).State == StateReady || len(e.rt.Ctrs) != 0 {
		t.Fatalf("readiness failure reported as success: %v", err)
	}
}

// Past the drain deadline a container whose API does not answer is stopped.
func TestForcedStopWithDeadAPI(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.s.EnsureRunning(ctx, uid1)
	clock := time.Now()
	e.s.now = func() time.Time { return clock }
	e.oc.ActiveDown = true
	e.s.Drain(uid1)
	e.s.Reap(ctx)
	if e.s.Status(uid1).State != StateDraining {
		t.Fatal("before the drain cap a failed probe must keep the container")
	}
	clock = clock.Add(3 * time.Hour)
	e.s.Reap(ctx)
	if e.s.Status(uid1).State != StateAbsent {
		t.Fatal("forced stop skipped on a failed probe")
	}
}

type failedStartCleanup struct {
	runtime.Runtime
	cancel  context.CancelFunc
	t       *testing.T
	removed bool
}

func (r *failedStartCleanup) Start(context.Context, string) error {
	r.cancel()
	return context.Canceled
}
func (r *failedStartCleanup) Remove(ctx context.Context, id string) error {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 10*time.Second || ctx.Err() != nil {
		r.t.Fatal("failed start cleanup must survive request cancellation with a bounded deadline")
	}
	r.removed = true
	return r.Runtime.Remove(ctx, id)
}
func TestFailedStartCleanupHasDeadlineAfterCancellation(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &failedStartCleanup{Runtime: e.rt, cancel: cancel, t: t}
	e.s.rt = probe
	if _, err := e.s.EnsureRunning(ctx, uid1); err == nil || !probe.removed {
		t.Fatalf("err=%v removed=%v", err, probe.removed)
	}
}
