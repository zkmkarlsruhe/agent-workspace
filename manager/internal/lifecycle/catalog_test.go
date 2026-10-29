// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"slices"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

func TestCatalogMountAndAfterStart(t *testing.T) {
	e := setup(t)
	e.s.cfg.CatalogDir = "/srv/opencode/base/catalog"
	var started []string
	e.s.AfterStart = func(_ context.Context, uid string) {
		if _, ok := e.s.Client(uid); !ok {
			t.Error("hook ran before the container counts as running")
		}
		started = append(started, uid)
	}
	if _, err := e.s.EnsureRunning(context.Background(), uid1); err != nil {
		t.Fatal(err)
	}
	ms := e.rt.Specs[0].Mounts
	if last := ms[len(ms)-1]; last != (runtime.Mount{Source: "/srv/opencode/base/catalog", Target: "/opt/workspace/catalog", ReadOnly: true}) {
		t.Fatalf("mounts %+v", ms)
	}
	if !slices.Contains(e.rt.Specs[0].Env, "OPENCODE_CONFIG=/home/user/.config/opencode/catalog.json") {
		t.Fatalf("env %v", e.rt.Specs[0].Env)
	}
	// an already running container is not started again
	e.s.EnsureRunning(context.Background(), uid1)
	if len(started) != 1 || started[0] != uid1 {
		t.Fatalf("hook calls %v", started)
	}
}
