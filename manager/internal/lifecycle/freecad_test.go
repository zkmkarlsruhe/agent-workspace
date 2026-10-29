// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type cadRuntime struct {
	runtime.Runtime
	configure func(io.Reader) error
}

func (r cadRuntime) Exec(ctx context.Context, n string, cmd []string, in io.Reader, out, stderr io.Writer) (int, error) {
	if len(cmd) == 2 && cmd[1] == "/opt/workspace-freecad/configure.py" {
		return 0, r.configure(in)
	}
	return r.Runtime.Exec(ctx, n, cmd, in, out, stderr)
}
func (r cadRuntime) name(ctx context.Context, id string) string {
	list, _ := r.Runtime.List(ctx)
	for _, c := range list {
		if c.ID == id {
			return c.Name
		}
	}
	return id
}
func (r cadRuntime) Start(ctx context.Context, id string) error {
	return r.Runtime.Start(ctx, r.name(ctx, id))
}
func (r cadRuntime) Stop(ctx context.Context, id string, d time.Duration) error {
	return r.Runtime.Stop(ctx, r.name(ctx, id), d)
}
func (r cadRuntime) Remove(ctx context.Context, id string) error {
	return r.Runtime.Remove(ctx, r.name(ctx, id))
}
func (r cadRuntime) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	return r.Runtime.Inspect(ctx, r.name(ctx, id))
}
func TestFreeCADLifecycle(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.s.cfg.FreeCADImage = "cad@sha256:" + strings.Repeat("a", 64)
	var mu sync.Mutex
	allowed, saveFailure := true, false
	token := ""
	saves := 0
	e.s.FreeCADAllowed = func(context.Context, string, string) bool { mu.Lock(); defer mu.Unlock(); return allowed }
	for i, folder := range []string{"first", "second"} {
		if err := e.s.CreateProject(ctx, uid1, NewProject{Name: folder}); err != nil {
			t.Fatal(err)
		}
		e.oc.Sessions = append(e.oc.Sessions, ocapi.Session{ID: []string{"ses_00000001", "ses_00000002"}[i], Location: ocapi.Location{Directory: e.s.projectDir(folder)}})
	}
	e.s.rt = cadRuntime{Runtime: e.rt, configure: func(in io.Reader) error {
		var v struct{ Token string }
		err := json.NewDecoder(in).Decode(&v)
		mu.Lock()
		token = v.Token
		mu.Unlock()
		return err
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if token == "" || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		var req struct{ Params struct{ Name string } }
		_ = json.NewDecoder(r.Body).Decode(&req)
		fail := false
		if req.Params.Name == "freecad_save_all" {
			saves++
			fail = saveFailure
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"content": []any{}, "isError": fail}})
	}))
	defer server.Close()
	defer e.s.Close()
	e.s.cadURL = func(int) string { return server.URL }
	open := func(chat string) string {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			v, err := e.s.CallFreeCAD(ctx, uid1, chat, "freecad_open", json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(v.StructuredContent)
			var out struct {
				Status  string
				Session string `json:"session_id"`
				URL     string `json:"browser_url"`
			}
			_ = json.Unmarshal(b, &out)
			if out.Status == "ready" {
				if !strings.Contains(out.URL, out.Session) {
					t.Fatal(out)
				}
				return out.Session
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("start timeout")
		return ""
	}
	first := open("ses_00000001")
	if open("ses_00000001") != first {
		t.Fatal("not reused")
	}
	target, ok := e.s.FreeCADTarget(ctx, uid1, first)
	if !ok {
		t.Fatal("missing route")
	}
	if _, ok = e.s.FreeCADTarget(ctx, "another", first); ok {
		t.Fatal("foreign owner")
	}
	if _, err := e.s.CallFreeCAD(ctx, uid1, "ses_00000002", "freecad_state", json.RawMessage(`{"session_id":"`+first+`"}`)); !errors.Is(err, ErrFreeCAD) {
		t.Fatal("foreign project", err)
	}
	bound, release, ok := target.Bind(ctx)
	if !ok {
		t.Fatal("lease")
	}
	go func() { <-bound.Done(); release() }()
	second := open("ses_00000002")
	if second == first {
		t.Fatal("reused project session")
	}
	select {
	case <-bound.Done():
	default:
		t.Fatal("old browser remains live")
	}
	if _, ok = e.s.FreeCADTarget(ctx, uid1, first); ok {
		t.Fatal("old URL remains live")
	}
	w := e.s.cadSlot(uid1).worker
	if err := e.s.rt.Remove(ctx, w.id); err != nil {
		t.Fatal(err)
	}
	restarted := open("ses_00000002")
	if restarted == second {
		t.Fatal("crashed application reused")
	}
	if _, ok = e.s.FreeCADTarget(ctx, uid1, second); ok {
		t.Fatal("crashed session URL accepted")
	}
	second = restarted
	mu.Lock()
	saveFailure = true
	mu.Unlock()
	args := json.RawMessage(`{"session_id":"` + second + `"}`)
	if _, err := e.s.CallFreeCAD(ctx, uid1, "ses_00000002", "freecad_close", args); err == nil {
		t.Fatal("closed unsaved edit")
	}
	if _, ok = e.s.FreeCADTarget(ctx, uid1, second); !ok {
		t.Fatal("failed close lost session")
	}
	mu.Lock()
	saveFailure = false
	allowed = false
	mu.Unlock()
	if _, ok = e.s.FreeCADTarget(ctx, uid1, second); ok {
		t.Fatal("revoked browser allowed")
	}
	if _, err := e.s.CallFreeCAD(ctx, uid1, "ses_00000002", "freecad_state", args); !errors.Is(err, ErrFreeCAD) {
		t.Fatal("revoked tools allowed", err)
	}
	e.s.sweepCAD(ctx)
	e.s.cadSlot(uid1).Lock()
	remaining := e.s.cadSlot(uid1).worker
	e.s.cadSlot(uid1).Unlock()
	if remaining != nil {
		t.Fatal("revoked worker remains")
	}
	mu.Lock()
	defer mu.Unlock()
	if saves < 3 {
		t.Fatal("missing save before teardown", saves)
	}
}
