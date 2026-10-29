// SPDX-License-Identifier: MIT

package lifecycle

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

const toolImg = "registry.example.org/operations/opencode-tool-medien:1@sha256:f6511c1c2ea3b3e7b1fecbffd5176a932fcb36ec1ea3ce6ed1dbae5afc9eec87"

func TestRunToolbox(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.rt.Root = t.TempDir() // the fake exec runs on the host with /workspace mapped here
	os.MkdirAll(e.rt.Root+"/bericht/medien", 0o755)
	run := func(dir, command string) (string, int, bool, error) {
		var out bytes.Buffer
		code, cut, err := e.s.RunToolbox(ctx, uid1, "media", toolImg, dir, command, &out)
		return out.String(), code, cut, err
	}
	if _, _, _, err := run("/workspace/bericht", "true"); err != ErrNotRunning {
		t.Fatalf("asleep: %v", err)
	}
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	out, code, cut, err := run("/workspace/bericht/medien", `echo hallo; echo fehler >&2; pwd`)
	if err != nil || code != 0 || cut || !strings.HasPrefix(out, "hallo\n") || !strings.Contains(out, "fehler\n") ||
		!strings.HasSuffix(out, "/bericht/medien\n") {
		t.Fatalf("run: %q %d %v %v", out, code, cut, err)
	}
	var tool runtime.Spec
	for _, s := range e.rt.Specs {
		if s.Tool {
			tool = s
		}
	}
	if tool.Name != "oc-user1-tb-media" || tool.Image != toolImg || tool.Network != "" || tool.Labels["oc.toolbox"] != "media" ||
		tool.Labels["oc.uid"] != uid1 || len(tool.Mounts) != 1 ||
		tool.Mounts[0] != (runtime.Mount{Source: "/srv/opencode/users/" + uid1 + "/workspace", Target: "/workspace"}) {
		t.Fatalf("tool spec %+v", tool)
	}
	if out, code, cut, _ := run("/workspace/bericht", "head -c 100000 /dev/zero"); !cut || len(out) != ToolboxOutput || code != 0 {
		t.Fatalf("cap: %d bytes cut=%v code %d", len(out), cut, code)
	}
	if _, code, _, _ := run("/workspace/bericht", "exit 3"); code != 3 {
		t.Fatalf("exit code %d", code)
	}
	if _, code, _, _ := run("/workspace/fehlt", "true"); code != 126 {
		t.Fatalf("missing dir: %d", code)
	}
	for _, dir := range []string{"/etc", "/workspace/../etc", "/workspacex", "relativ"} {
		if _, _, _, err := run(dir, "true"); err == nil {
			t.Errorf("dir %q accepted", dir)
		}
	}
	creates := len(e.rt.Specs)
	run("/workspace", "true")
	if len(e.rt.Specs) != creates {
		t.Fatal("running tool container created again")
	}
	// a new catalog image replaces the container
	var out2 bytes.Buffer
	if _, _, err := e.s.RunToolbox(ctx, uid1, "media", toolImg[:len(toolImg)-1]+"0", "/workspace", "true", &out2); err != nil ||
		len(e.rt.Specs) != creates+1 || !slices.Contains(e.rt.Removed, "oc-user1-tb-media") {
		t.Fatalf("image change: %v specs %d removed %v", err, len(e.rt.Specs), e.rt.Removed)
	}
	if err := e.s.Stop(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rt.Inspect(ctx, "oc-user1-tb-media"); err != runtime.ErrNotFound {
		t.Fatalf("tool container survived the stop: %v", err)
	}
}

func TestDeleteAccountRemovesTools(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.rt.Root = t.TempDir()
	e.s.PurgeData = func(context.Context, string, int) error { return nil }
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.RunToolbox(ctx, uid1, "media", toolImg, "/workspace", "true", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.DeleteAccount(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if len(e.rt.Ctrs) != 0 {
		t.Fatalf("left behind: %v", e.rt.Ctrs)
	}
}
