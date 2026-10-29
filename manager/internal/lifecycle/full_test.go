// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

const uid2 = "288992ff55666970b6ddc030565d3c4c9452a9543cc3718d1b1ad5e52523ece4"

func TestMaxRunning(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.s.cfg.MaxRunning = 1
	if _, err := e.st.EnsureUser(ctx, identity.Identity{UID: uid2, Username: "anna"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.EnsureRunning(ctx, uid2); !errors.Is(err, ErrFull) {
		t.Fatalf("second person: %v, want ErrFull", err)
	}
	if len(e.rt.Specs) != 1 {
		t.Fatalf("a refused start must not create a container: %d creates", len(e.rt.Specs))
	}
	// the person whose container runs is not affected
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	// the portal start shows "full", without an error id, and the next try starts
	e.s.StartAsync(context.Background(), uid2)
	waitState(t, e.s, uid2, EnvFull)
	if st, _ := e.s.envState(ctx, uid2); st.ErrorID != "" {
		t.Fatalf("full is no error: %+v", st)
	}
	if err := e.s.Stop(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	e.s.StartAsync(context.Background(), uid2)
	waitState(t, e.s, uid2, EnvRunning)
}

func waitState(t *testing.T, s *Service, uid string, want EnvState) {
	t.Helper()
	for range 200 {
		if st, _ := s.envState(context.Background(), uid); st.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := s.envState(context.Background(), uid)
	t.Fatalf("state %s, want %s", st.State, want)
}

func TestStartReleasesPlaceBeforeAfterStart(t *testing.T) {
	e := setupInMemory(t)
	e.s.cfg.MaxRunning = 2
	ctx := context.Background()
	if _, err := e.st.EnsureUser(ctx, identity.Identity{UID: uid2, Username: "anna"}); err != nil {
		t.Fatal(err)
	}
	e.s.AfterStart = func(ctx context.Context, uid string) {
		if uid != uid1 {
			return
		}
		e.s.mu.Lock()
		creating := e.s.creating
		e.s.mu.Unlock()
		if creating != 0 || e.s.Running() != 1 {
			t.Errorf("ready container counted twice: running %d, creating %d", e.s.Running(), creating)
		}
		if _, err := e.s.EnsureRunning(ctx, uid2); err != nil {
			t.Errorf("free place refused during AfterStart: %v", err)
		}
	}
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if e.s.creating != 0 || e.s.Running() != 2 {
		t.Fatalf("reservation leaked or released twice: running %d, creating %d", e.s.Running(), e.s.creating)
	}
}
