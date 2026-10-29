// SPDX-License-Identifier: MIT

package lifecycle

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle/lctest"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestIsolatedProjectLifecycleWithoutSharedRuntime(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.EnsureUser(ctx, identity.Identity{UID: uid1, Username: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AcceptConsent(ctx, uid1, consent.Version(), time.Now()); err != nil {
		t.Fatal(err)
	}
	d := instance.Current()
	def, _ := d.Assistant("it-hilfe")
	p := store.Project{ID: "private-project", UID: uid1, Folder: "support", Description: "Private support", Assistant: d.AssistantPin(def), CreatedAt: time.Now()}
	if err := st.CreateAssistantProject(ctx, p, false); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAssistantSession(ctx, uid1, p.ID, "ses_profile_test", time.Now()); err != nil {
		t.Fatal(err)
	}
	se, err := st.AssistantSession(ctx, uid1, "ses_profile_test")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Minute)
	if err := st.ClaimAssistantTurn(ctx, se, until); err != nil {
		t.Fatal(err)
	}
	account, err := st.AssistantAccount(ctx, uid1)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitAssistantTurn(ctx, se, p.Assistant, account, consent.Version(), until, []byte(`[{"role":"user","content":"private support text"}]`)); err != nil {
		t.Fatal(err)
	}
	rt := lctest.NewRuntime()
	s := New(config.Config{}, rt, st, slog.New(slog.DiscardHandler))
	projects, err := s.ListProjects(ctx, uid1)
	if err != nil || len(projects) != 1 || projects[0].Chats != 1 || projects[0].Assistant != p.Assistant {
		t.Fatalf("%+v %v", projects, err)
	}
	if starting, _, _, _ := s.StartState(uid1); starting {
		t.Fatal("started shared runtime")
	}
	var buf bytes.Buffer
	if err := s.Export(ctx, uid1, "support", &buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range zr.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		found = found || strings.Contains(string(b), "private support text")
	}
	if !found {
		t.Fatal("private chat missing from owner export")
	}
	if err := s.RenameProject(ctx, uid1, "support", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveToTrash(ctx, uid1, "renamed"); err != nil {
		t.Fatal(err)
	}
	trash, err := s.ListTrash(ctx, uid1)
	if err != nil || len(trash) != 1 || trash[0].Chats != 1 {
		t.Fatal(trash, err)
	}
	if _, err := st.AssistantSession(ctx, uid1, se.ID); err == nil {
		t.Fatal("trashed chat still active")
	}
	if err := s.Restore(ctx, uid1, trash[0].ID, "restored"); err != nil {
		t.Fatal(err)
	}
	got, err := st.ProjectByFolder(ctx, uid1, "restored")
	if err != nil || !got.Isolated || got.Assistant != p.Assistant {
		t.Fatal(got, err)
	}
	if err := s.MoveToTrash(ctx, uid1, "restored"); err != nil {
		t.Fatal(err)
	}
	trash, _ = s.ListTrash(ctx, uid1)
	stale, err := st.TrashItem(ctx, uid1, trash[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Purge(ctx, uid1, trash[0].ID); err != nil {
		t.Fatal(err)
	}
	if sessions, err := st.AssistantSessions(ctx, uid1, p.ID); err != nil || len(sessions) != 0 {
		t.Fatal("private data survived purge", sessions, err)
	}
	if err := st.RestoreTrash(ctx, stale, "resurrected", time.Now()); err == nil {
		t.Fatal("stale restore resurrected purged data")
	}
	// Idle support projects expire without any shared environment ever running.
	p.ID, p.Folder = "idle-private", "idle-support"
	p.CreatedAt = time.Now().Add(-config.ProjectIdleTrash - time.Hour)
	if err := st.CreateAssistantProject(ctx, p, false); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAssistantSession(ctx, uid1, p.ID, "ses_profile_idle", time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.now = func() time.Time { return now }
	s.TrashIdle(ctx)
	expired, err := st.Trash(ctx, uid1)
	if err != nil || len(expired) != 1 || expired[0].Pending || expired[0].Chats != 1 {
		t.Fatal(expired, err)
	}
	now = now.Add(config.TrashRetention + time.Hour)
	s.TrashIdle(ctx)
	if sessions, err := st.AssistantSessions(ctx, uid1, p.ID); err != nil || len(sessions) != 0 {
		t.Fatal("retention kept private chat", sessions, err)
	}

}
