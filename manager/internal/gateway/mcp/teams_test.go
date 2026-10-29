// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestKnowledgeInvitedSourceBindingRefused(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "teams.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	var actors []store.KnowledgeActor
	var users []store.User
	for i, name := range []string{"owner", "reader"} {
		u, err := st.EnsureUser(ctx, identity.Identity{UID: strings.Repeat(string(rune('a'+i)), 64), Username: name})
		if err != nil {
			t.Fatal(err)
		}
		if err = st.AcceptConsent(ctx, u.UID, consent.Version(), time.Now()); err != nil {
			t.Fatal(err)
		}
		actor, err := st.KnowledgeActor(ctx, u.UID)
		if err != nil {
			t.Fatal(err)
		}
		users = append(users, u)
		actors = append(actors, actor)
	}
	team, err := st.CreateTeam(ctx, actors[0], "Team", "")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := st.CreateKnowledgeCollection(ctx, actors[0], team.ID, "Hidden collection", "")
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.SaveKnowledgePage(ctx, actors[0], store.KnowledgePage{ID: uuid.NewString(), CollectionID: collection.ID, Title: "Hidden page", Body: "needle"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	project := store.Project{ID: uuid.NewString(), UID: actors[1].UID, Folder: "sources", CreatedAt: time.Now()}
	if err = st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	invite := func() int {
		t.Helper()
		current, _, _, err := st.Team(ctx, actors[0], team.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = st.SetTeamMember(ctx, actors[0], team.ID, users[1].UID, users[1].Incarnation, "member", current.Revision); err != nil {
			t.Fatal(err)
		}
		return current.Revision + 1
	}
	revision := invite()
	if err = st.BindKnowledge(ctx, actors[1], project.ID, collection.ID, true); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("invited binding", err)
	}
	if err = st.RespondTeamInvitation(ctx, actors[1], team.ID, true, revision); err != nil {
		t.Fatal(err)
	}
	if err = st.BindKnowledge(ctx, actors[1], project.ID, collection.ID, true); err != nil {
		t.Fatal(err)
	}
	server := &Server{Knowledge: st, KnowledgeSession: func(context.Context, store.User, string) (string, error) { return project.ID, nil }}
	read, _ := json.Marshal(map[string]string{"collection": collection.ID, "page": page.ID})
	if _, meta := server.knowledgeCall(ctx, users[1], "session", "knowledge_read", read); meta.status != 200 {
		t.Fatal("accepted read", meta.status)
	}
	if err = st.SetTeamMember(ctx, actors[1], team.ID, users[1].UID, "", "", revision+1); err != nil {
		t.Fatal(err)
	}
	invite()
	// The project still pins this collection, but the fresh invitation grants nothing.
	result, meta := server.knowledgeCall(ctx, users[1], "session", "knowledge_list", json.RawMessage(`{}`))
	body, _ := json.Marshal(result)
	if meta.status != 200 || strings.Contains(string(body), collection.ID) || strings.Contains(string(body), collection.Title) {
		t.Fatal("invited source listed", string(body), meta.status)
	}
	if _, meta = server.knowledgeCall(ctx, users[1], "session", "knowledge_read", read); meta.status != 404 {
		t.Fatal("invited read", meta.status)
	}
	search, _ := json.Marshal(map[string]string{"collection": collection.ID, "query": "needle"})
	if _, meta = server.knowledgeCall(ctx, users[1], "session", "knowledge_search", search); meta.status != 404 {
		t.Fatal("invited search", meta.status)
	}
}
