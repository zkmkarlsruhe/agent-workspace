// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle/lctest"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

type probeTransport struct {
	http.RoundTripper
	fail   bool
	probes int
}

func (p *probeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if p.fail {
		p.probes++
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Millisecond)
		defer cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.RoundTripper.RoundTrip(r)
}

func TestProbeTimeoutKeepsContainers(t *testing.T) {
	for _, budget := range []bool{false, true} {
		t.Run(strconv.FormatBool(budget), func(t *testing.T) {
			e := setupInMemory(t)
			ctx := context.Background()
			u2, err := e.st.EnsureUser(ctx, identity.Identity{UID: strings.Repeat("b", 64), Username: "other"})
			if err != nil {
				t.Fatal(err)
			}
			for slot, uid := range map[int]string{1: uid1, u2.Slot: u2.UID} {
				name := config.SlotContainer(slot)
				e.rt.Ctrs[name] = &runtime.Container{ID: name, Name: name, Running: true,
					Labels: map[string]string{"oc.uid": uid, "oc.slot": strconv.Itoa(slot), "oc.generation": "5", runtime.LabelManaged: "1"}}
			}
			probe := &probeTransport{RoundTripper: assistantTransport{e.oc}, fail: true}
			e.s.http = &http.Client{Transport: probe}
			rctx := ctx
			if budget {
				var cancel context.CancelFunc
				rctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer cancel()
			}
			if err := e.s.Reconcile(rctx); err != nil {
				t.Fatal(err)
			}
			if len(e.rt.Ctrs) != 2 || len(e.rt.Removed) != 0 || e.s.get(uid1) != nil || e.s.get(u2.UID) != nil {
				t.Fatal("unanswered probes must leave both containers for a later retry")
			}
			if !budget && probe.probes != 2 {
				t.Fatal("reconcile did not continue after a probe failure")
			}
			if _, err := e.s.EnsureRunning(ctx, uid1); err == nil || len(e.rt.Removed) != 0 {
				t.Fatal("start must return the probe error without removing the container", err)
			}
			probe.fail = false
			if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
				t.Fatal(err)
			}
			if e.s.get(uid1).gen != 5 || len(e.rt.Specs) != 0 {
				t.Fatal("retry must adopt the existing generation")
			}
		})
	}
}

type cancelAfterMove struct {
	*lctest.Runtime
	cancel context.CancelFunc
}

func (f *cancelAfterMove) Exec(ctx context.Context, name string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	code, err := f.Runtime.Exec(ctx, name, cmd, stdin, stdout, stderr)
	if code == 0 && err == nil && f.cancel != nil && strings.Contains(cmd[2], "mv -T") {
		f.cancel()
		f.cancel = nil
	}
	return code, err
}

func TestTrashStoreFailureAfterMove(t *testing.T) {
	for _, operation := range []string{"trash", "restore", "restore-renamed"} {
		t.Run(operation, func(t *testing.T) {
			e := setupInMemory(t)
			ctx := context.Background()
			if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "project"}); err != nil {
				t.Fatal(err)
			}
			e.oc.Sessions = []ocapi.Session{{ID: "ses_saved", Location: ocapi.Location{Directory: e.s.projectDir("project")}}}
			var id string
			if operation != "trash" {
				if err := e.s.MoveToTrash(ctx, uid1, "project"); err != nil {
					t.Fatal(err)
				}
				items, err := e.st.Trash(ctx, uid1)
				if err != nil || len(items) != 1 {
					t.Fatal(items, err)
				}
				id = items[0].ID
			}
			failed, cancel := context.WithCancel(ctx)
			defer cancel()
			e.s.rt = &cancelAfterMove{e.rt, cancel}
			if operation == "restore-renamed" {
				e.s.rt = &cancelAfterMove{e.rt, func() {
					e.s.now = func() time.Time {
						cancel() // RestoreTrash reads the clock after moving the sessions.
						return time.Now()
					}
				}}
			}
			var err error
			name := "project"
			if operation == "trash" {
				err = e.s.MoveToTrash(failed, uid1, name)
			} else {
				if operation == "restore-renamed" {
					name = "renamed"
				}
				err = e.s.Restore(failed, uid1, id, name)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("store operation must fail after the move: %v", err)
			}
			items, err := e.st.Trash(ctx, uid1)
			if err != nil || len(items) != 1 || items[0].Pending != (operation == "trash") {
				t.Fatalf("recoverable trash row: %+v, %v", items, err)
			}
			id = items[0].ID
			if _, err := os.Stat(e.s.trashDir(id)); err != nil {
				t.Fatal("folder must remain in trash", err)
			}
			if _, err := os.Stat(e.s.projectDir(name)); !os.IsNotExist(err) {
				t.Fatal("failed operation left a live folder", err)
			}
			if ps, err := e.st.Projects(ctx, uid1); err != nil || len(ps) != 0 {
				t.Fatal("failed operation left a live project row", ps, err)
			}
			e.s.finishPending(ctx, uid1)
			if err := e.s.Restore(ctx, uid1, id, ""); err != nil {
				t.Fatal("retry must restore the project", err)
			}
			if _, err := os.Stat(e.s.projectDir("project")); err != nil {
				t.Fatal(err)
			}
			if _, err := e.st.ProjectByFolder(ctx, uid1, "project"); err != nil {
				t.Fatal(err)
			}
			if items, err := e.st.Trash(ctx, uid1); err != nil || len(items) != 0 {
				t.Fatal(items, err)
			}
			if sessions, err := e.s.Sessions(ctx, uid1, "project"); err != nil || len(sessions) != 1 {
				t.Fatal("restored sessions must remain attached", sessions, err)
			}
		})
	}
}

func TestActivityDuringCollectKeepsContainer(t *testing.T) {
	for _, activity := range []string{"touch", "active", "probe-error"} {
		t.Run(activity, func(t *testing.T) {
			e := setupInMemory(t)
			ctx := context.Background()
			if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "project"}); err != nil {
				t.Fatal(err)
			}
			clock := time.Now().Add(time.Hour)
			e.s.now = func() time.Time { return clock }
			e.s.CollectChats = true
			collected := false
			e.s.http = &http.Client{Transport: assistantTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/session" {
					collected = true
					clock = clock.Add(time.Second)
					switch activity {
					case "touch":
						e.s.Touch(uid1)
					case "active":
						e.oc.Active = []string{"ses_new"}
					case "probe-error":
						e.oc.ActiveDown = true
					}
				}
				e.oc.ServeHTTP(w, r)
			})}}
			e.s.Reap(ctx)
			if !collected || e.s.Status(uid1).State != StateReady || len(e.rt.Removed) != 0 {
				t.Fatal("activity during collection must prevent the idle stop")
			}
		})
	}
}
