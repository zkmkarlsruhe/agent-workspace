// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
)

func TestRenameProject(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, n := range []string{"alt", "belegt"} {
		if err := e.s.CreateProject(ctx, uid1, NewProject{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.CreateSession(ctx, uid1, "alt", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RenameProject(ctx, uid1, "alt", "belegt"); !errors.Is(err, ErrExists) {
		t.Fatalf("taken name: %v", err)
	}
	e.oc.Active = []string{"ses_1"}
	if err := e.s.RenameProject(ctx, uid1, "alt", "neu"); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	e.oc.Active = nil
	if err := e.s.RenameProject(ctx, uid1, "alt", "neu"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.oc.Evicted, []string{e.s.projectDir("alt")}) {
		t.Fatalf("old location not evicted: %v", e.oc.Evicted)
	}
	if !slices.Equal(e.oc.Moved, []string{"ses_1=" + e.s.projectDir("neu")}) {
		t.Fatalf("moved %v", e.oc.Moved)
	}
	if _, err := os.Stat(e.s.projectDir("alt")); !os.IsNotExist(err) {
		t.Fatal("old folder must be gone")
	}
	if _, err := os.Stat(e.s.projectDir("neu")); err != nil {
		t.Fatal("new folder must exist", err)
	}
	if _, err := e.st.ProjectByFolder(ctx, uid1, "neu"); err != nil {
		t.Fatal("store must know the new name", err)
	}
	if ss, _ := e.s.Sessions(ctx, uid1, "neu"); len(ss) != 1 {
		t.Fatal("chat must list under the new name")
	}
	if err := e.s.RenameProject(ctx, uid1, "fehlt", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}
