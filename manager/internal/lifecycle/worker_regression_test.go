// SPDX-License-Identifier: MIT

package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

type cadRecoveryRuntime struct {
	runtime.Runtime
	root string
}

func (r cadRecoveryRuntime) Exec(ctx context.Context, id string, cmd []string, in io.Reader, out, stderr io.Writer) (int, error) {
	if len(cmd) == 2 && cmd[1] == "/opt/workspace-freecad/recovery.py" {
		c := exec.CommandContext(ctx, "python3", "-B", "../../../services/freecad/recovery.py")
		c.Env = append(os.Environ(), "WORKSPACE_FREECAD_PROJECT="+r.root)
		c.Stdout, c.Stderr = out, stderr
		if err := c.Run(); err != nil {
			return 1, err
		}
		return 0, nil
	}
	return r.Runtime.Exec(ctx, id, cmd, in, out, stderr)
}

func readyCAD(t *testing.T) (env, *cadWorker) {
	t.Helper()
	e := setupInMemory(t)
	ctx := context.Background()
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "drawing"}); err != nil {
		t.Fatal(err)
	}
	p, err := e.st.ProjectByFolder(ctx, uid1, "drawing")
	if err != nil {
		t.Fatal(err)
	}
	r := e.s.get(uid1)
	e.s.cfg.FreeCADImage = "cad"
	e.s.FreeCADAllowed = func(context.Context, string, string) bool { return true }
	e.oc.Sessions = []ocapi.Session{{ID: "ses_cad00001", Location: ocapi.Location{Directory: e.s.projectDir(p.Folder)}}}
	w := &cadWorker{id: "cad-worker", endpoint: "http://cad/mcp", session: cadID(), project: p.ID, folder: p.Folder, slot: r.slot, generation: r.gen, lease: newLease(), done: make(chan struct{})}
	close(w.done)
	e.s.cadSlot(uid1).worker = w
	e.rt.Ctrs[w.id] = &runtime.Container{ID: w.id, Name: w.id, Running: true, Labels: map[string]string{"oc.uid": uid1, "oc.freecad": "1"}}
	e.s.rt = cadRecoveryRuntime{e.rt, e.s.projectDir(p.Folder)}
	e.s.cadTransport = assistantTransport{http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		io.WriteString(out, `{"result":{"isError":true,"content":[]}}`)
	})}
	t.Cleanup(e.s.Close)
	return e, w
}

func TestCADForcedTeardownRecovery(t *testing.T) {
	for _, mode := range []string{"expiry", "idle", "stop", "restart", "revocation", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			e, w := readyCAD(t)
			root := e.s.projectDir(w.folder)
			if err := os.WriteFile(filepath.Join(root, "drawing.FCStd"), []byte("saved document"), 0600); err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			e.s.log = slog.New(slog.NewTextHandler(&log, nil))
			ctx := context.Background()
			switch mode {
			case "expiry":
				r := e.s.get(uid1)
				now := r.started.Add(e.s.cfg.MaxLifetime + e.s.cfg.DrainCap + time.Minute)
				e.s.now = func() time.Time { return now }
				e.s.Reap(ctx)
			case "idle":
				now := e.s.get(uid1).lastRequest.Add(e.s.cfg.IdleTimeout + time.Minute)
				e.s.now = func() time.Time { return now }
				e.s.Reap(ctx)
			case "stop":
				if err := e.s.Stop(ctx, uid1); err != nil {
					t.Fatal(err)
				}
			case "restart":
				if _, err := e.s.Restart(ctx, uid1); err != nil {
					t.Fatal(err)
				}
			case "revocation":
				e.s.FreeCADAllowed = func(context.Context, string, string) bool { return false }
				e.s.sweepCAD(ctx)
			case "shutdown":
				e.s.Close()
			}
			if e.s.cadSlot(uid1).worker != nil {
				t.Fatal("CAD still tracked")
			}
			if _, err := e.rt.Inspect(ctx, w.id); !errors.Is(err, runtime.ErrNotFound) {
				t.Fatal("worker survives", err)
			}
			copies, err := filepath.Glob(filepath.Join(root, "drawing.recovery-*.FCStd"))
			if err != nil || len(copies) != 1 {
				t.Fatalf("recovery copies %v: %v", copies, err)
			}
			data, err := os.ReadFile(copies[0])
			if err != nil || string(data) != "saved document" {
				t.Fatal("recovery corrupted", err)
			}
			if !strings.Contains(log.String(), "FreeCAD save failed") {
				t.Fatal("missing save failure log")
			}
			if (mode == "expiry" || mode == "idle") && e.s.get(uid1) != nil {
				t.Fatal("main runtime survives expiry")
			}
		})
	}
}

func TestCADUserCloseRefusesSaveFailure(t *testing.T) {
	e, w := readyCAD(t)
	_, err := e.s.CallFreeCAD(context.Background(), uid1, "ses_cad00001", "freecad_close", json.RawMessage(`{"session_id":"`+w.session+`"}`))
	if err == nil || e.s.cadSlot(uid1).worker != w {
		t.Fatal("failed save discarded worker", err)
	}
	_, release, ok := w.lease.bind(context.Background())
	if !ok {
		t.Fatal("close refusal killed lease")
	}
	release()
}

func TestCADCallDoesNotHoldPersonLock(t *testing.T) {
	e, w := readyCAD(t)
	entered := make(chan struct{})
	e.s.cadTransport = assistantTransport{http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		var v struct{ Params struct{ Name string } }
		_ = json.NewDecoder(req.Body).Decode(&v)
		if v.Params.Name == "freecad_state" {
			close(entered)
			<-req.Context().Done()
		}
		io.WriteString(out, `{"result":{"content":[]}}`)
	})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.s.CallFreeCAD(context.Background(), uid1, "ses_cad00001", "freecad_state", json.RawMessage(`{"session_id":"`+w.session+`"}`))
	}()
	<-entered
	lock := e.s.lock(uid1)
	if !lock.TryLock() {
		t.Fatal("worker round trip holds person lock")
	}
	lock.Unlock()
	if _, err := e.s.CallFreeCAD(context.Background(), uid1, "ses_cad00001", "freecad_state", json.RawMessage(`{"session_id":"`+w.session+`"}`)); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent worker call admitted", err)
	}
	if err := e.s.Stop(context.Background(), uid1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("active call not drained")
	}
}

func TestCADCloseWaitsForStartupPublication(t *testing.T) {
	e, w := readyCAD(t)
	w.done = make(chan struct{})
	w.id = ""
	ctx, release, ok := w.lease.bind(context.Background())
	if !ok {
		t.Fatal("lease")
	}
	go func() { <-ctx.Done(); w.id = "cad-worker"; w.err = ErrFreeCAD; release(); close(w.done) }()
	if err := e.s.closePersonCAD(context.Background(), uid1, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rt.Inspect(context.Background(), "cad-worker"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatal("startup worker leaked")
	}
}

type lateInspectRuntime struct {
	runtime.Runtime
	entered, finish chan struct{}
}

func (r lateInspectRuntime) Inspect(ctx context.Context, name string) (runtime.Container, error) {
	close(r.entered)
	<-r.finish
	return runtime.Container{}, runtime.ErrNotFound
}

func TestCloseDrainsLateFileWorkAndRefusesWorkers(t *testing.T) {
	e := setupInMemory(t)
	if _, err := e.s.EnsureRunning(context.Background(), uid1); err != nil {
		t.Fatal(err)
	}
	entered, finish := make(chan struct{}), make(chan struct{})
	e.s.rt = lateInspectRuntime{e.rt, entered, finish}
	done := make(chan error, 1)
	go func() { done <- e.s.sh(context.Background(), uid1, "true", nil, nil) }()
	<-entered
	closed := make(chan struct{})
	go func() { e.s.Close(); close(closed) }()
	<-e.s.background.Done()
	select {
	case <-closed:
		t.Fatal("Close did not join file operation")
	default:
	}
	close(finish)
	if err := <-done; !errors.Is(err, ErrNotRunning) {
		t.Fatal(err)
	}
	<-closed
	if err := e.s.sh(context.Background(), uid1, "true", nil, nil); !errors.Is(err, ErrNotRunning) {
		t.Fatal("exec admitted after Close")
	}
	if _, err := e.s.CallFreeCAD(context.Background(), uid1, "", "freecad_open", nil); !errors.Is(err, ErrNotRunning) {
		t.Fatal("CAD admitted after Close")
	}
	if _, err := e.s.CallOffice(context.Background(), uid1, "office_open", nil); !errors.Is(err, ErrNotRunning) {
		t.Fatal("Office admitted after Close")
	}
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { e.s.suspectAsync(uid1) })
	}
	wg.Wait()
	e.s.workers.Wait()
	if len(e.rt.Removed) != 0 {
		t.Fatal("late suspect ran after Close")
	}
}

func TestCADSaveTimeoutIsBounded(t *testing.T) {
	e, _ := readyCAD(t)
	e.s.cadTransport = assistantTransport{http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Error("save/recovery not bounded")
		}
		io.WriteString(out, `{"result":{"isError":true,"content":[]}}`)
	})}
	if err := e.s.Stop(context.Background(), uid1); err != nil {
		t.Fatal(err)
	}
}

func TestOfficeRemovalFailureRetries(t *testing.T) {
	e := setupInMemory(t)
	ctx := context.Background()
	// An in-memory SDK connection exercises the real closed-session path without a listener.
	server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	ct, st := sdk.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "fixture", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := "office-worker"
	e.rt.Ctrs[id] = &runtime.Container{ID: id, Name: id, Running: true}
	w := &officeWorker{id: id, session: cs, transport: &http.Transport{}, lease: newLease()}
	e.s.officeSlot(uid1).worker = w
	e.rt.RemoveErr = map[string]error{id: errors.New("temporary remove failure")}
	if err := e.s.CloseOffice(ctx, uid1); err == nil || !w.failed {
		t.Fatal("failed removal left usable record", err)
	}
	delete(e.rt.RemoveErr, id)
	e.s.sweepOffice(ctx)
	if e.s.officeSlot(uid1).worker != nil {
		t.Fatal("failed worker did not retry")
	}
	if _, err := e.rt.Inspect(ctx, id); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatal("failed worker leaked")
	}
}

func TestCADHungSaveTimeout(t *testing.T) {
	e, _ := readyCAD(t)
	e.s.cadTransport = assistantTransport{http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		var v struct{ Params struct{ Name string } }
		_ = json.NewDecoder(req.Body).Decode(&v)
		if v.Params.Name == "freecad_save_all" {
			<-req.Context().Done()
			out.WriteHeader(504)
			return
		}
		io.WriteString(out, `{"result":{"content":[]}}`)
	})}
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if err := e.s.closePersonCAD(context.Background(), uid1, true); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Fatalf("save timeout: %s", elapsed)
		}
	})
}

func TestOfficeStartCancelledByClose(t *testing.T) {
	e := setupInMemory(t)
	if _, err := e.s.EnsureRunning(context.Background(), uid1); err != nil {
		t.Fatal(err)
	}
	e.s.cfg.OfficeImage = "office"
	entered := make(chan struct{})
	e.s.OfficeCreds = func(ctx context.Context, uid string) (string, string, error) {
		close(entered)
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.s.CallOffice(context.Background(), uid1, "office_open", json.RawMessage(`{}`))
		done <- err
	}()
	<-entered
	e.s.Close()
	if err := <-done; err == nil {
		t.Fatal("Office start continued during shutdown")
	}
	if e.s.officeSlot(uid1).worker != nil || len(e.rt.Specs) != 1 {
		t.Fatal("late Office worker created")
	}
}

func TestCADSweepKeepsWorkerOnDatabaseFailure(t *testing.T) {
	e, w := readyCAD(t)
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	e.s.FreeCADAllowed = func(context.Context, string, string) bool { return false }
	e.s.sweepCAD(context.Background())
	if e.s.cadSlot(uid1).worker != w {
		t.Fatal("database failure forced teardown")
	}
}
