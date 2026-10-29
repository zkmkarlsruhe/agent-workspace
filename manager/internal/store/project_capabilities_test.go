// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func TestProjectCapabilitiesPersistence(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	u, err := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	p := Project{ID: "project", UID: u.UID, Folder: "research", CreatedAt: time.Now()}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := s.ProjectByID(ctx, u.UID, p.ID)
	if err != nil || got.Capabilities != nil {
		t.Fatal("legacy must inherit", got, err)
	}
	if err := s.SetProjectCapabilities(ctx, uid(2), p.Folder, []string{"rooms"}); err != ErrNotFound {
		t.Fatal("foreign owner", err)
	}
	for _, ids := range [][]string{{}, {"workspace-knowledge"}} {
		if err := s.SetProjectCapabilities(ctx, u.UID, p.Folder, ids); err != nil {
			t.Fatal(err)
		}
		got, err = s.ProjectByFolder(ctx, u.UID, p.Folder)
		if err != nil || got.Capabilities == nil || !slices.Equal(got.Capabilities, ids) {
			t.Fatal(got, err)
		}
	}
	if err := s.RenameProject(ctx, p.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	// Deliberately pass the stale project snapshot: the transaction must preserve the latest selection.
	p.Folder = "renamed"
	if err := s.MoveToTrash(ctx, p, "trash", time.Now(), time.Now().Add(time.Hour), nil, false, false); err != nil {
		t.Fatal(err)
	}
	trash, err := s.TrashItem(ctx, u.UID, "trash")
	if err != nil || !slices.Equal(trash.Project.Capabilities, []string{"workspace-knowledge"}) {
		t.Fatal(trash, err)
	}
	if err := s.RestoreTrash(ctx, trash, "restored", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = s.ProjectByFolder(ctx, u.UID, "restored")
	if err != nil || !slices.Equal(got.Capabilities, []string{"workspace-knowledge"}) {
		t.Fatal(got, err)
	}
}
