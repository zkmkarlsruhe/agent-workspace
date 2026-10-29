// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestCreateSessionAppliesOnlyOpenedProject(t *testing.T) {
	e := setupInMemory(t)
	ctx := context.Background()
	e.s.set(uid1, &running{slot: 1, started: time.Now()})
	for _, name := range []string{"one", "two", "three"} {
		if err := e.st.CreateProject(ctx, store.Project{ID: name, UID: uid1, Folder: name, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	var projects []string
	cat := &catalog.Service{Dir: "../../../deploy/catalog", Store: e.st, Projects: e.st, Client: e.s.Client, Log: e.s.log,
		Shell: func(_ context.Context, _, script string, _ io.Writer, args ...string) error {
			if strings.Contains(script, ".opencode-capabilities.tmp") {
				projects = append(projects, args[0])
			}
			return nil
		},
	}
	e.s.PrepareCapabilities = cat.Prepare
	if _, err := e.s.CreateSession(ctx, uid1, "two", ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(projects, []string{"/workspace/two"}) {
		t.Fatal("project execs", projects)
	}
	projects = nil
	if _, err := cat.SetProject(ctx, uid1, "three", []string{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(projects, []string{"/workspace/three"}) {
		t.Fatal("toggle execs", projects)
	}
	projects = nil
	if _, err := cat.Apply(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if len(projects) != 3 {
		t.Fatal("full apply skipped projects", projects)
	}
}
