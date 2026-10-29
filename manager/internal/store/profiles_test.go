// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func TestAssistantPinOwnershipAndTrash(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, id := range []string{uid(1), uid(2)} {
		if _, err := s.EnsureUser(ctx, identity.Identity{UID: id, Username: id}); err != nil {
			t.Fatal(err)
		}
	}
	p := Project{ID: "p", UID: uid(1), Folder: "support", CreatedAt: time.Now()}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.SharedRuntimeAccess(ctx, p.UID); err != nil {
		t.Fatal(err)
	}
	stale := p
	pin := assistants.Pin{ID: "support", Version: 1, Hash: strings.Repeat("a", 64)}
	if err := s.PinProjectAssistant(ctx, uid(2), p.Folder, pin); err == nil {
		t.Fatal("foreign owner pinned")
	}
	for range 2 {
		if err := s.PinProjectAssistant(ctx, p.UID, p.Folder, pin); err != nil {
			t.Fatal(err)
		}
	}
	for _, changed := range []assistants.Pin{{}, {ID: "support", Version: 2, Hash: pin.Hash}, {ID: "other", Version: 1, Hash: pin.Hash}, {ID: "support", Version: 1, Hash: strings.Repeat("b", 64)}} {
		if err := s.PinProjectAssistant(ctx, p.UID, p.Folder, changed); err == nil {
			t.Fatal("pin changed")
		}
	}
	p, err := s.ProjectByFolder(ctx, p.UID, p.Folder)
	if err != nil || p.Assistant != pin {
		t.Fatalf("pin lost: %+v %v", p, err)
	}
	if !errors.Is(s.SharedRuntimeAccess(ctx, p.UID), assistants.ErrIsolationRequired) {
		t.Fatal("shared access allowed")
	}
	if err := s.SharedRuntimeAccess(ctx, uid(2)); err != nil {
		t.Fatal("other owner denied")
	}
	now := time.Now()
	if err := s.MoveToTrash(ctx, stale, "trash", now, now.Add(time.Hour), nil, false, false); err != nil {
		t.Fatal(err)
	}
	item, err := s.TrashItem(ctx, p.UID, "trash")
	if err != nil || item.Project.Assistant != pin {
		t.Fatalf("trash pin lost: %+v %v", item, err)
	}
	if !errors.Is(s.SharedRuntimeAccess(ctx, p.UID), assistants.ErrIsolationRequired) {
		t.Fatal("trash bypass")
	}
	if err := s.RestoreTrash(ctx, item, "renamed", now); err != nil {
		t.Fatal(err)
	}
	p, err = s.ProjectByFolder(ctx, p.UID, "renamed")
	if err != nil || p.Assistant != pin {
		t.Fatalf("restored pin lost: %+v %v", p, err)
	}
}

func TestOpenCodeAssistantAdmissionSurvivesTrash(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "owner"}); err != nil {
		t.Fatal(err)
	}
	p := Project{ID: "new-profile", UID: uid(1), Folder: "support", CreatedAt: time.Now(), Assistant: assistants.Pin{ID: "support", Version: 1, Hash: strings.Repeat("a", 64)}}
	if err := s.CreateOpenCodeAssistantProject(ctx, p); err == nil {
		t.Fatal("created without consent")
	}
	if err := s.AcceptConsent(ctx, p.UID, consent.Version(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOpenCodeAssistantProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.SharedRuntimeAccess(ctx, p.UID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.MoveToTrash(ctx, p, "trash", now, now.Add(time.Hour), nil, false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SharedRuntimeAccess(ctx, p.UID); err != nil {
		t.Fatal("trash blocked runtime", err)
	}
	item, err := s.TrashItem(ctx, p.UID, "trash")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreTrash(ctx, item, "restored", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SharedRuntimeAccess(ctx, p.UID); err != nil {
		t.Fatal("restore lost admission", err)
	}
	p, err = s.ProjectByFolder(ctx, p.UID, "restored")
	if err != nil || p.Isolated {
		t.Fatal(p, err)
	}
	if err := s.MoveToTrash(ctx, p, "trash", now, now.Add(time.Hour), nil, false, false); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTrash(ctx, p.UID, "trash"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM opencode_profile_projects`).Scan(&count); err != nil || count != 0 {
		t.Fatal("purge left admission", count, err)
	}
}
