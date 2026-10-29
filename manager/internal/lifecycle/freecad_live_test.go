// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"encoding/json"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The main chat API is a fixture; application creation, GUI, MCP, persistence and
// removal use the real engine. The opt-in workspace must exist on the Docker host.
type liveCADRuntime struct {
	runtime.Runtime
	app  runtime.Runtime
	main string
}

func (r liveCADRuntime) isApp(n string) bool { return n == "oc-user201-freecad" || len(n) == 64 }
func (r liveCADRuntime) Create(ctx context.Context, s runtime.Spec) error {
	if s.FreeCAD {
		return r.app.Create(ctx, s)
	}
	return r.Runtime.Create(ctx, s)
}
func (r liveCADRuntime) Inspect(ctx context.Context, n string) (runtime.Container, error) {
	if r.isApp(n) {
		return r.app.Inspect(ctx, n)
	}
	if n == "oc-user201" {
		n = r.main
	}
	return r.Runtime.Inspect(ctx, n)
}
func (r liveCADRuntime) Start(ctx context.Context, n string) error {
	if r.isApp(n) {
		return r.app.Start(ctx, n)
	}
	return r.Runtime.Start(ctx, n)
}
func (r liveCADRuntime) Stop(ctx context.Context, n string, d time.Duration) error {
	if r.isApp(n) {
		return r.app.Stop(ctx, n, d)
	}
	return r.Runtime.Stop(ctx, n, d)
}
func (r liveCADRuntime) Remove(ctx context.Context, n string) error {
	if r.isApp(n) {
		return r.app.Remove(ctx, n)
	}
	return r.Runtime.Remove(ctx, n)
}
func (r liveCADRuntime) Exec(ctx context.Context, n string, cmd []string, in io.Reader, out, stderr io.Writer) (int, error) {
	if r.isApp(n) {
		return r.app.Exec(ctx, n, cmd, in, out, stderr)
	}
	return r.Runtime.Exec(ctx, r.main, cmd, in, out, stderr)
}
func TestFreeCADLive(t *testing.T) {
	image, root := os.Getenv("OC_FREECAD_IT_IMAGE"), os.Getenv("OC_FREECAD_IT_ROOT")
	if image == "" || root == "" {
		t.Skip("set OC_FREECAD_IT_IMAGE and OC_FREECAD_IT_ROOT; requires local Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	real, err := runtime.NewDocker()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := real.NetworkGatewayMode(ctx, "oc-u201"); err != runtime.ErrNotFound {
		t.Fatal("test slot 201 already exists", err)
	}
	if err := real.EnsureNetwork(ctx, "oc-u201", netip.MustParsePrefix("10.199.201.0/24"), "nat"); err != nil {
		t.Fatal(err)
	}
	defer real.RemoveNetwork(context.Background(), "oc-u201")
	e := setup(t)
	if err := os.MkdirAll(root, 0750); err != nil {
		t.Fatal(err)
	}
	root, err = os.MkdirTemp(root, "run-")
	if err != nil {
		t.Fatal(err)
	}
	e.s.cfg.FreeCADImage = image
	e.s.cfg.UserNetPrefix = "10.199"
	e.s.cfg.UsersRoot = root
	e.s.ws = filepath.Join(root, uid1, "workspace")
	if err := os.MkdirAll(e.s.ws, 0750); err != nil {
		t.Fatal(err)
	}
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "live-test"}); err != nil {
		t.Fatal(err)
	}
	r := e.s.get(uid1)
	main := config.SlotContainer(r.slot)
	r.slot = 201
	e.s.rt = liveCADRuntime{Runtime: e.rt, app: real, main: main}
	defer e.s.Close()
	e.s.FreeCADAllowed = func(context.Context, string, string) bool { return true }
	e.oc.Sessions = append(e.oc.Sessions, ocapi.Session{ID: "ses_live1234", Location: ocapi.Location{Directory: e.s.projectDir("live-test")}})
	open := func() string {
		for ctx.Err() == nil {
			v, err := e.s.CallFreeCAD(ctx, uid1, "ses_live1234", "freecad_open", json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(v.StructuredContent)
			var out struct {
				Status  string
				Session string `json:"session_id"`
			}
			_ = json.Unmarshal(b, &out)
			if out.Status == "ready" {
				return out.Session
			}
			time.Sleep(time.Second)
		}
		t.Fatal(ctx.Err())
		return ""
	}
	session := open()
	target, ok := e.s.FreeCADTarget(ctx, uid1, session)
	if !ok {
		t.Fatal("browser missing")
	}
	resp, err := http.Get(target.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(strings.ToLower(string(b)), "html") {
		t.Fatal("desktop unavailable", resp.StatusCode)
	}
	name := "Live_" + cadID()[:8]
	args, _ := json.Marshal(map[string]string{"session_id": session, "action": "new", "name": name})
	result, err := e.s.CallFreeCAD(ctx, uid1, "ses_live1234", "freecad_document", args)
	if err != nil || result.IsError {
		t.Fatal("document", err, result)
	}
	if _, err = e.s.CallFreeCAD(ctx, uid1, "ses_live1234", "freecad_close", json.RawMessage(`{"session_id":"`+session+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = real.Inspect(ctx, "oc-user201-freecad"); err != runtime.ErrNotFound {
		t.Fatal("container remains", err)
	}
	second := open()
	if second == session {
		t.Fatal("session reused after restart")
	}
	result, err = e.s.CallFreeCAD(ctx, uid1, "ses_live1234", "freecad_state", json.RawMessage(`{"session_id":"`+second+`"}`))
	if err != nil || result.IsError {
		t.Fatal(err, result)
	}
	b, _ = json.Marshal(result)
	if !strings.Contains(string(b), name) {
		t.Fatal("native document not restored", string(b))
	}
	if _, ok = e.s.FreeCADTarget(ctx, uid1, session); ok {
		t.Fatal("stale browser accepted")
	}
	t.Log("Manager start/reuse, real browser HTTP, authenticated MCP, save/stop/remove, recreate/restore and stale URL rejection passed")
}
