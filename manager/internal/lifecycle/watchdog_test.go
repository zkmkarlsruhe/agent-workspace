// SPDX-License-Identifier: MIT

package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
)

func TestWatchdogStopOwnerProjectAndSettlement(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.s.cfg.WatchdogSilent = time.Second
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"test", "other"} {
		if err := e.s.CreateProject(ctx, uid1, NewProject{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(1000, 0)
	e.s.now = func() time.Time { return now }
	var logs bytes.Buffer
	e.s.log = slog.New(logx.NewHandler(slog.NewJSONHandler(&logs, nil)))
	e.oc.Sessions = []ocapi.Session{watchSession("root", ""), watchSession("child", "root")}
	for i := range e.oc.Sessions {
		e.oc.Sessions[i].Location.Directory = e.s.projectDir("test")
		e.oc.Sessions[i].Title = "private title"
	}
	e.oc.Active = []string{"root", "child"}
	d := e.s.get(uid1).watch
	d.connection(true)
	e.s.refreshWatchdog(ctx, e.s.client(e.s.get(uid1)), d)
	now = now.Add(2 * time.Second)
	if n, err := e.s.Watchdog(ctx, uid1, "test"); err != nil || len(n) != 2 {
		t.Fatal(n, err)
	}
	e.s.Watchdog(ctx, uid1, "test")
	if strings.Count(logs.String(), `"op":"watchdog_detect"`) != 2 || strings.Contains(logs.String(), "private title") {
		t.Fatal(logs.String())
	}
	if n, err := e.s.Watchdog(ctx, uid2, ""); err != nil || len(n) != 0 {
		t.Fatal("other owner's notices", n, err)
	}
	for _, tc := range []struct{ uid, project, session string }{
		{uid2, "", "child"}, {uid1, "other", "child"}, {uid1, "test", "absent"}, {uid1, "test", "../child"},
	} {
		if err := e.s.StopSilentSession(ctx, tc.uid, tc.project, tc.session); err == nil {
			t.Fatal("accepted", tc)
		}
	}
	if len(e.oc.Interrupted) != 0 {
		t.Fatal(e.oc.Interrupted)
	}
	e.oc.ActiveDown = true
	if err := e.s.StopSilentSession(ctx, uid1, "test", "child"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("failed active check must refuse stop", err)
	}
	e.oc.ActiveDown = false
	e.oc.FailInterrupt = true
	if err := e.s.StopSilentSession(ctx, uid1, "test", "child"); err == nil {
		t.Fatal("interrupt failure swallowed")
	}
	if n, _ := e.s.Watchdog(ctx, uid1, "test"); len(n) != 2 {
		t.Fatal("failed interrupt cleared notice", n)
	}
	e.oc.FailInterrupt = false
	e.oc.InterruptPending = true
	if err := e.s.StopSilentSession(ctx, uid1, "test", "child"); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.s.Watchdog(ctx, uid1, "test"); len(n) != 2 {
		t.Fatal("acceptance is not settlement", n)
	}
	e.oc.Emit("session.execution.interrupted", map[string]any{"sessionID": "child"})
	e.s.refreshWatchdog(ctx, e.s.client(e.s.get(uid1)), d)
	e.oc.InterruptPending = false
	if !slices.Equal(e.oc.Active, []string{"root"}) {
		t.Fatal("child stop affected parent", e.oc.Active)
	}
	if n, _ := e.s.Watchdog(ctx, uid1, "test"); len(n) != 1 || n[0].SessionID != "root" {
		t.Fatal(n)
	}
	// The same action supports roots; it still sends exactly one interrupt.
	if err := e.s.StopSilentSession(ctx, uid1, "test", "root"); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.String(), `"op":"watchdog_stop"`) != 3 || strings.Contains(logs.String(), "private title") {
		t.Fatal(logs.String())
	}
	if n, _ := e.s.Watchdog(ctx, uid1, "test"); len(n) != 0 {
		t.Fatal(n)
	}
}

func TestWatchdogPendingQuestionAndResumedSessionCannotStop(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.s.cfg.WatchdogSilent = time.Second
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	e.s.now = func() time.Time { return now }
	e.oc.Sessions, e.oc.Active = []ocapi.Session{watchSession("root", "")}, []string{"root"}
	r := e.s.get(uid1)
	r.watch.connection(true)
	e.s.refreshWatchdog(ctx, e.s.client(r), r.watch)
	now = now.Add(2 * time.Second)
	e.oc.Forms = 1
	if err := e.s.StopSilentSession(ctx, uid1, "", "root"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	e.oc.Forms = 0
	e.s.refreshWatchdog(ctx, e.s.client(r), r.watch)
	now = now.Add(2 * time.Second)
	r.watch.event(watchdogEvent("session.text.delta", "root", nil), now)
	if err := e.s.StopSilentSession(ctx, uid1, "", "root"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if len(e.oc.Interrupted) != 0 {
		t.Fatal(e.oc.Interrupted)
	}
}
