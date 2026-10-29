// SPDX-License-Identifier: MIT

package lifecycle

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestKnowledgeSessionBoundary(t *testing.T) {
	e := setupInMemory(t)
	ctx := context.Background()
	e.s.set(uid1, &running{slot: 1, started: time.Now()})
	if err := e.st.CreateProject(ctx, store.Project{ID: "notes", UID: uid1, Folder: "notes", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	u, err := e.st.UserByUID(ctx, uid1)
	if err != nil {
		t.Fatal(err)
	}
	e.oc.Sessions = []ocapi.Session{{ID: "ses_root12345", Location: ocapi.Location{Directory: path.Join(e.s.ws, "notes")}},
		{ID: "ses_child1234", ParentID: "ses_root12345", Location: ocapi.Location{Directory: path.Join(e.s.ws, "notes")}},
		{ID: "ses_support12", Agent: "profile-support", Location: ocapi.Location{Directory: path.Join(e.s.ws, "notes")}},
		{ID: "ses_wrongdir1", Location: ocapi.Location{Directory: path.Join(e.s.ws, "other")}}}
	if p, err := e.s.KnowledgeSession(ctx, u, "ses_root12345"); err != nil || p != "notes" {
		t.Fatal(p, err)
	}
	for _, session := range []string{"ses_child1234", "ses_support12", "ses_wrongdir1", "missing"} {
		if _, err := e.s.KnowledgeSession(ctx, u, session); err == nil {
			t.Fatal(session)
		}
	}
	u.Incarnation = "stale"
	if _, err := e.s.KnowledgeSession(ctx, u, "ses_root12345"); err == nil {
		t.Fatal("stale account")
	}
}

func TestKnowledgeAccountExportWithoutRuntime(t *testing.T) {
	e := setupInMemory(t)
	ctx := context.Background()
	if err := e.st.AcceptConsent(ctx, uid1, consent.Version(), time.Now()); err != nil {
		t.Fatal(err)
	}
	a, err := e.st.KnowledgeActor(ctx, uid1)
	if err != nil {
		t.Fatal(err)
	}
	personal, err := e.st.CreateKnowledgeCollection(ctx, a, "", "Personal", "")
	if err != nil {
		t.Fatal(err)
	}
	team, err := e.st.CreateTeam(ctx, a, "Team", "")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := e.st.CreateKnowledgeCollection(ctx, a, team.ID, "Team knowledge", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []store.KnowledgeCollection{personal, shared} {
		if _, err := e.st.SaveKnowledgePage(ctx, a, store.KnowledgePage{ID: uuid.NewString(), CollectionID: c.ID, Title: "Evidence", Body: c.Title}, 0); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := e.s.Export(ctx, uid1, "", &buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "knowledge/") {
			if !strings.Contains(f.Name, personal.ID) {
				t.Fatal("team copied into personal export", f.Name)
			}
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(r)
			r.Close()
			if err != nil || !bytes.Contains(body, []byte("Personal")) {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing knowledge export")
	}
	if _, running := e.s.Target(uid1); running {
		t.Fatal("export started runtime")
	}
}
