// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestKnowledgeToolsSelectionAndWrites(t *testing.T) {
	raw, e := os.ReadFile("../../../../instance/minimal/instance.yaml")
	if e != nil {
		t.Fatal(e)
	}
	def, e := instance.Parse([]byte(strings.Replace(string(raw), "knowledge: false", "knowledge: true", 1)))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(instance.Select(def))
	st, e := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	u, e := st.EnsureUser(ctx, identity.Identity{UID: strings.Repeat("a", 64), Username: "owner"})
	if e != nil {
		t.Fatal(e)
	}
	if e = st.AcceptConsent(ctx, u.UID, consent.Version(), time.Now()); e != nil {
		t.Fatal(e)
	}
	a, e := st.KnowledgeActor(ctx, u.UID)
	if e != nil {
		t.Fatal(e)
	}
	c, e := st.CreateKnowledgeCollection(ctx, a, "", "Selected", "")
	if e != nil {
		t.Fatal(e)
	}
	unbound, e := st.CreateKnowledgeCollection(ctx, a, "", "Not selected", "")
	if e != nil {
		t.Fatal(e)
	}
	p := store.Project{ID: uuid.NewString(), UID: u.UID, Folder: "notes", CreatedAt: time.Now()}
	if e = st.CreateProject(ctx, p); e != nil {
		t.Fatal(e)
	}
	if e = st.BindKnowledge(ctx, a, p.ID, c.ID, true); e != nil {
		t.Fatal(e)
	}
	revoked := false
	s := New(Server{Users: st, Knowledge: st, Usage: &rec{}, Log: slog.New(slog.DiscardHandler), SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf,
		KnowledgeSession: func(_ context.Context, got store.User, session string) (string, error) {
			if revoked || got.Incarnation != u.Incarnation || session != "ses_knowledge1234" {
				return "", store.ErrNotFound
			}
			return p.ID, nil
		}})
	list := rpc(s, "10.90.1.10:40000", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(list.Body.String(), `"knowledge_create"`) {
		t.Fatal(list.Body.String())
	}
	call := func(name string, args map[string]any, session string) string {
		t.Helper()
		body, e := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args, "_meta": map[string]string{"ai.opencode/sessionID": session}}})
		if e != nil {
			t.Fatal(e)
		}
		return rpc(s, "10.90.1.10:40000", string(body)).Body.String()
	}
	session := "ses_knowledge1234"
	result := call("knowledge_list", map[string]any{}, session)
	if !strings.Contains(result, "Selected") || strings.Contains(result, "Not selected") {
		t.Fatal(result)
	}
	args := map[string]any{"collection": c.ID, "page": uuid.NewString(), "title": "Instructions", "body": "Facts", "sources": "Source"}
	for range 2 {
		result = call("knowledge_create", args, session)
		if strings.Contains(result, `"isError":true`) {
			t.Fatal(result)
		}
	}
	rows, e := st.KnowledgePages(ctx, a, c.ID)
	if e != nil || len(rows) != 1 || rows[0].Revision != 1 {
		t.Fatal(rows, e)
	}
	args["expected_revision"] = 1
	args["body"] = "Updated"
	if result = call("knowledge_update", args, session); strings.Contains(result, `"isError":true`) {
		t.Fatal(result)
	}
	if result = call("knowledge_update", args, session); !strings.Contains(result, `"isError":true`) {
		t.Fatal("stale edit", result)
	}
	args["expected_revision"] = 2
	delete(args, "sources")
	if result = call("knowledge_update", args, session); !strings.Contains(result, `"isError":true`) {
		t.Fatal("missing sources cleared", result)
	}
	read := map[string]any{"collection": c.ID, "page": rows[0].ID}
	result = call("knowledge_read", read, session)
	if !strings.Contains(result, "https://apps.example.org/knowledge/") || !strings.Contains(result, "Updated") {
		t.Fatal(result)
	}

	t.Run("update_preserves_metadata", func(t *testing.T) {
		child, err := st.SaveKnowledgePage(ctx, a, store.KnowledgePage{ID: uuid.NewString(), CollectionID: c.ID, Title: "Archived child", Body: "Before", Parent: rows[0].ID, Archived: true}, 0)
		if err != nil {
			t.Fatal(err)
		}
		update := map[string]any{"collection": c.ID, "page": child.ID, "expected_revision": 1, "title": child.Title, "body": "After", "sources": "Source"}
		if result := call("knowledge_update", update, session); strings.Contains(result, `"isError":true`) {
			t.Fatal(result)
		}
		saved, err := st.KnowledgePage(ctx, a, c.ID, child.ID, 0)
		if err != nil || saved.Parent != child.Parent || !saved.Archived || saved.Body != "After" {
			t.Fatal(saved, err)
		}
		update["expected_revision"], update["parent"], update["archived"] = 2, "", false
		if result := call("knowledge_update", update, session); strings.Contains(result, `"isError":true`) {
			t.Fatal(result)
		}
		saved, err = st.KnowledgePage(ctx, a, c.ID, child.ID, 0)
		if err != nil || saved.Parent != "" || saved.Archived {
			t.Fatal(saved, err)
		}
	})
	read["collection"] = unbound.ID
	if result = call("knowledge_read", read, session); !strings.Contains(result, `"isError":true`) {
		t.Fatal("unbound", result)
	}
	read["collection"] = c.ID
	for _, se := range []string{"", "ses_forged1234"} {
		if result = call("knowledge_read", read, se); !strings.Contains(result, `"isError":true`) {
			t.Fatal("forged session", result)
		}
	}
	restricted := context.WithValue(ctx, assistantScopeKey{}, assistantScope{project: p.ID, pin: assistants.Pin{ID: "support"}})
	if ok, _ := s.gate(restricted, u)("knowledge_read"); ok {
		t.Fatal("support gate")
	}
	resultBytes, _ := json.Marshal(read)
	res, e := s.callTool(restricted, u, &sdk.CallToolParamsRaw{Name: "knowledge_read", Arguments: resultBytes})
	if e == nil && !res.IsError {
		t.Fatal("restricted tool accepted")
	}
	if e = st.BindKnowledge(ctx, a, p.ID, c.ID, false); e != nil {
		t.Fatal(e)
	}
	if result = call("knowledge_read", read, session); !strings.Contains(result, `"isError":true`) {
		t.Fatal("unbound read", result)
	}
	revoked = true
	if result = call("knowledge_list", map[string]any{}, session); !strings.Contains(result, `"isError":true`) {
		t.Fatal("revoked session", result)
	}
}
