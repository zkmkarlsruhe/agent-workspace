// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle/lctest"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type (
	fakeRT = lctest.Runtime
	fakeOC = lctest.OpenCode
)

const uid1 = "a7889ac42c1d44753644ec24acde57e8650e05b30d4b86f39dc0c96a98d25356"

type env struct {
	s  *Service
	rt *fakeRT
	oc *fakeOC
	st *store.Store
}

func setup(t *testing.T) env {
	t.Helper()
	e := setupInMemory(t)
	srv := httptest.NewServer(e.oc)
	t.Cleanup(srv.Close)
	e.s.http.Transport = nil
	e.s.baseURL = func(int) string { return srv.URL }
	return e
}

func setupInMemory(t *testing.T) env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.EnsureUser(context.Background(), identity.Identity{UID: uid1, Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Domain: "apps.example.org", UsersRoot: "/srv/opencode/users", RuntimeImage: "img:1", AdminConfig: "opencode-config",
		ManagerContainer: "oc-manager", EgressContainer: "egress", UserNetPrefix: "10.90",
		MasterKey: []byte("0123456789abcdef0123456789abcdef"), StartTimeout: 2 * time.Second,
		IdleTimeout: 30 * time.Minute, TabCap: 4 * time.Hour, DrainCap: 2 * time.Hour, MaxLifetime: 24 * time.Hour,
	}
	oc := &fakeOC{}
	rt := lctest.NewRuntime()
	s := New(cfg, rt, st, slog.New(slog.DiscardHandler))
	s.http.Transport = assistantTransport{oc}
	s.ws = t.TempDir()
	s.interruptWait = 0
	return env{s, rt, oc, st}
}

func TestEnsureRunningOnceAndSpec(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(e.rt.Specs) != 1 {
		t.Fatalf("want exactly one create, got %d", len(e.rt.Specs))
	}
	sp := e.rt.Specs[0]
	if sp.Name != "oc-user1" || sp.Network != "oc-u1" || sp.IP.String() != "10.90.1.10" {
		t.Fatalf("%+v", sp)
	}
	if sp.Labels["oc.uid"] != uid1 || sp.Labels["oc.generation"] != "1" || sp.Labels[runtime.LabelManaged] != "1" {
		t.Fatalf("labels %v", sp.Labels)
	}
	if !slices.Contains(sp.Env, "OPENCODE_PASSWORD="+Password(e.s.cfg.MasterKey, uid1, 1)) ||
		!slices.Contains(sp.Env, "HTTPS_PROXY=http://egress:3128") {
		t.Fatalf("env %v", sp.Env)
	}
	if !slices.Equal(sp.Cmd, []string{"--cors", "https://alice.apps.example.org", "--print-logs", "--log-level", "info"}) {
		t.Fatal(sp.Cmd)
	}
	if len(sp.Mounts) != 5 || sp.Mounts[0].Source != "/srv/opencode/users/"+uid1+"/workspace" || !sp.Mounts[4].ReadOnly {
		t.Fatalf("mounts %+v", sp.Mounts)
	}
	if !slices.Contains(e.rt.Nets, "oc-u1=10.90.1.0/24") ||
		!slices.Contains(e.rt.Connects, "oc-u1/oc-manager/10.90.1.3/[llm-gw]") ||
		!slices.Contains(e.rt.Connects, "oc-u1/egress/10.90.1.2/[egress]") {
		t.Fatalf("nets %v connects %v", e.rt.Nets, e.rt.Connects)
	}
	if tg, ok := e.s.Target(uid1); !ok || tg.Password != Password(e.s.cfg.MasterKey, uid1, 1) {
		t.Fatal("target")
	}
	// restart: new generation, new password
	if _, err := e.s.Restart(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if tg, _ := e.s.Target(uid1); tg.Password != Password(e.s.cfg.MasterKey, uid1, 2) {
		t.Fatal("restart must bump the generation")
	}
	if err := e.s.Stop(ctx, uid1); err != nil || e.s.Status(uid1).State != StateAbsent || len(e.rt.Ctrs) != 0 {
		t.Fatal("stop", err)
	}
}

func TestReconcile(t *testing.T) {
	e := setup(t)
	e.rt.Ctrs["oc-user1"] = &runtime.Container{ID: "x1", Name: "oc-user1", Running: true,
		Labels: map[string]string{"oc.uid": uid1, "oc.slot": "1", "oc.generation": "5", runtime.LabelManaged: "1"}}
	e.rt.Ctrs["oc-user9"] = &runtime.Container{ID: "x9", Name: "oc-user9", Running: true,
		Labels: map[string]string{"oc.uid": strings.Repeat("b", 64), "oc.slot": "9", "oc.generation": "1"}}
	if err := e.s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tg, ok := e.s.Target(uid1); !ok || tg.Password != Password(e.s.cfg.MasterKey, uid1, 5) {
		t.Fatal("running container must be adopted with the label generation")
	}
	if _, ok := e.rt.Ctrs["oc-user9"]; ok {
		t.Fatal("orphan must be removed")
	}
	if len(e.rt.Specs) != 0 {
		t.Fatal("reconcile must not create")
	}
}

func TestDecide(t *testing.T) {
	c := config.Config{IdleTimeout: 30 * time.Minute, TabCap: 4 * time.Hour, MaxLifetime: 24 * time.Hour}
	now := time.Now()
	cases := []struct {
		name string
		r    running
		want action
	}{
		{"recent request", running{started: now.Add(-time.Hour), lastRequest: now.Add(-10 * time.Minute)}, keep},
		{"idle", running{started: now.Add(-time.Hour), lastRequest: now.Add(-31 * time.Minute)}, stopIfIdle},
		{"open tab", running{started: now.Add(-time.Hour), lastRequest: now.Add(-2 * time.Hour), streams: 1}, keep},
		{"forgotten tab", running{started: now.Add(-5 * time.Hour), lastRequest: now.Add(-5 * time.Hour), streams: 1}, stopIfIdle},
		{"lifetime", running{started: now.Add(-25 * time.Hour), lastRequest: now}, beginDrain},
		{"draining", running{draining: true, lastRequest: now}, drainOrKill},
	}
	for _, tc := range cases {
		if got := decide(now, tc.r, c); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

func TestReap(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.s.EnsureRunning(ctx, uid1)
	clock := time.Now().Add(time.Hour)
	e.s.now = func() time.Time { return clock }

	e.oc.Active = []string{"ses_x"}
	e.s.Reap(ctx)
	if e.s.Status(uid1).State != StateReady {
		t.Fatal("running agent must keep the container")
	}
	e.s.Drain(uid1)
	clock = clock.Add(3 * time.Hour) // past DrainCap
	e.s.Reap(ctx)
	if e.s.Status(uid1).State != StateAbsent || len(e.oc.Interrupted) != 1 {
		t.Fatalf("drain cap must interrupt and stop: %v", e.oc.Interrupted)
	}

	e.s.EnsureRunning(ctx, uid1)
	clock = clock.Add(time.Hour)
	e.s.Reap(ctx)
	if e.s.Status(uid1).State != StateAbsent {
		t.Fatal("idle container must stop")
	}
}

// Past MaxLifetime + DrainCap the container stops in that tick, even when its
// API hangs on session lists (pending trash) and interrupts and reports
// sessions as running; without draining ever having started.
func TestReapForcedStopNotPostponed(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "alt"}); err != nil {
		t.Fatal(err)
	}
	p, _ := e.st.ProjectByFolder(ctx, uid1, "alt")
	if err := e.st.MoveToTrash(ctx, p, "t1", time.Now(), time.Now().Add(time.Hour), nil, false, true); err != nil {
		t.Fatal(err)
	}
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method == "GET" && r.URL.Path == "/api/session") || strings.HasSuffix(r.URL.Path, "/interrupt") {
			<-r.Context().Done()
			return
		}
		e.oc.ServeHTTP(w, r)
	}))
	defer hang.Close()
	e.s.baseURL = func(int) string { return hang.URL }
	e.s.callTimeout = 200 * time.Millisecond
	e.oc.Active = []string{"ses_a", "ses_b", "ses_c"}
	clock := time.Now().Add(e.s.cfg.MaxLifetime + e.s.cfg.DrainCap + time.Minute)
	e.s.now = func() time.Time { return clock }
	t0 := time.Now()
	e.s.Reap(ctx)
	if e.s.Status(uid1).State != StateAbsent {
		t.Fatal("lifetime deadline passed, container still running")
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("forced stop took %v", d)
	}
}

func TestStreamsKeepAlive(t *testing.T) {
	e := setup(t)
	e.s.EnsureRunning(context.Background(), uid1)
	done := e.s.StreamOpened(uid1)
	if e.s.get(uid1).streams != 1 {
		t.Fatal("stream not counted")
	}
	done()
	done()
	if e.s.get(uid1).streams != 0 {
		t.Fatal("done must be idempotent")
	}
}

func TestProvisionBeforeCreate(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var calls []string
	fail := true
	e.s.Provision = func(_ context.Context, uid string, slot int) error {
		calls = append(calls, fmt.Sprintf("%s/%d", uid, slot))
		if fail {
			return errors.New("quota")
		}
		return nil
	}
	if _, err := e.s.EnsureRunning(ctx, uid1); err == nil || len(e.rt.Specs) != 0 {
		t.Fatalf("provision error must stop the create: %v, %d creates", err, len(e.rt.Specs))
	}
	fail = false
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil || len(e.rt.Specs) != 1 {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1] != uid1+"/1" {
		t.Fatal(calls)
	}
	if e.rt.Specs[0].Labels["oc.generation"] != "1" {
		t.Fatal("failed provision must not bump the generation")
	}
}

// A container removed from outside (docker rm) is noticed on the next use and
// replaced, instead of the first exec failing with "No such container".
func TestEnsureRunningReplacesRemovedContainer(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	e.rt.Mu.Lock()
	delete(e.rt.Ctrs, "oc-user1")
	e.rt.Mu.Unlock()
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if len(e.rt.Specs) != 2 || e.rt.Ctrs["oc-user1"] == nil || !e.rt.Ctrs["oc-user1"].Running {
		t.Fatalf("want a fresh container, specs %d", len(e.rt.Specs))
	}
	if tg, _ := e.s.Target(uid1); tg.Password != Password(e.s.cfg.MasterKey, uid1, 2) {
		t.Fatal("fresh container must get a new generation")
	}
	// a stopped container is replaced the same way; a running one is kept
	e.rt.Ctrs["oc-user1"].Running = false
	e.s.EnsureRunning(ctx, uid1)
	e.s.EnsureRunning(ctx, uid1)
	if len(e.rt.Specs) != 3 {
		t.Fatalf("specs %d, want 3", len(e.rt.Specs))
	}
}

// A file operation on a container removed from outside answers ErrNotRunning
// (portal: "Umgebung schläft") instead of 500, and the next start replaces it.
func TestExecOnRemovedContainer(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	e.rt.Mu.Lock()
	delete(e.rt.Ctrs, "oc-user1")
	e.rt.Ctrs["other"] = &runtime.Container{}
	e.rt.Mu.Unlock()
	if err := e.s.Shell(ctx, uid1, "true", nil); err != ErrNotRunning {
		t.Fatalf("want ErrNotRunning, got %v", err)
	}
	for range 100 {
		if e.s.get(uid1) == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e.s.get(uid1) != nil {
		t.Fatal("state of the removed container must be dropped")
	}
}

func TestReconcileNetworkIsolation(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(fmt.Sprint(running), func(t *testing.T) {
			e := setup(t)
			e.s.cfg.NetGatewayMode = "isolated"
			var logs strings.Builder
			e.s.log = slog.New(slog.NewTextHandler(&logs, nil))
			e.rt.GatewayModes = map[string]string{"oc-u1": ""}
			if running {
				e.rt.Ctrs["oc-user1"] = &runtime.Container{ID: "x1", Name: "oc-user1", Running: true,
					Labels: map[string]string{"oc.uid": uid1, "oc.slot": "1", "oc.generation": "5", runtime.LabelManaged: "1"}}
			}
			ctx := context.Background()
			if err := e.s.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if running {
				if len(e.rt.NetsGone) != 0 || e.rt.GatewayModes["oc-u1"] != "" {
					t.Fatal("running person's network changed")
				}
				if _, err := e.s.reconcileNetwork(ctx, 1); err != nil {
					t.Fatal(err)
				}
				if count := strings.Count(logs.String(), "network isolation deferred"); count != 1 {
					t.Fatalf("deferred notice logged %d times: %s", count, logs.String())
				}
				if err := e.s.Stop(ctx, uid1); err != nil {
					t.Fatal(err)
				}
				if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
					t.Fatal(err)
				}
			}
			if len(e.rt.NetsGone) != 1 || e.rt.GatewayModes["oc-u1"] != "isolated" || len(e.rt.Connects) < 2 {
				t.Fatalf("migration: removed=%v modes=%v connections=%v", e.rt.NetsGone, e.rt.GatewayModes, e.rt.Connects)
			}
		})
	}
}
