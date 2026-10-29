// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestProjectCapabilitiesDiscoveryAndDirectCall(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	uid := strings.Repeat("a", 64)
	if _, err := st.EnsureUser(ctx, identity.Identity{UID: uid, Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"rooms", "workspace-knowledge"} {
		if err := st.SetOptIn(ctx, uid, id, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []store.Project{
		{ID: "wiki", Folder: "wiki", Capabilities: []string{"workspace-knowledge"}},
		{ID: "other", Folder: "other", Capabilities: []string{"rooms"}},
	} {
		p.UID, p.CreatedAt = uid, time.Now()
		if err := st.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	cat := &catalog.Service{Dir: "../../../../deploy/catalog", Store: st, Projects: st, Log: slog.New(slog.DiscardHandler),
		Client: func(string) (*ocapi.Client, bool) { return nil, false },
		SessionDir: func(_ context.Context, _, session string) (string, error) {
			if session == "ses_wiki1234" {
				return "/workspace/wiki", nil
			}
			if session == "ses_other1234" {
				return "/workspace/other", nil
			}
			return "", store.ErrNotFound
		}}
	srv := New(Server{Users: st, Usage: st, Rooms: &Rooms{}, Log: slog.New(slog.DiscardHandler),
		SlotOf: func(netip.Addr) (int, bool) { return 1, true }, Grants: cat.Grants, ProjectAccess: cat.ProjectAccess, ProjectScoped: true})
	rpc := func(project, body string) string {
		t.Helper()
		req := httptest.NewRequest("POST", "/mcp/project/"+project+"/workspace", strings.NewReader(body))
		req.SetPathValue("capProject", project)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	call := func(session string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"rooms_free","arguments":{},"_meta":{"ai.opencode/sessionID":%q}}}`, session)
	}
	if got := rpc("wiki", list); strings.Contains(got, `"rooms_free"`) || strings.Contains(got, `"web_search"`) {
		t.Fatal(got)
	}
	for _, session := range []string{"ses_wiki1234", "ses_other1234", "", "ses_missing1234"} {
		got := rpc("wiki", call(session))
		if !strings.Contains(got, `"isError":true`) && !strings.Contains(got, `"error"`) {
			t.Fatal("direct call escaped", session, got)
		}
	}
	if _, err := cat.SetProject(ctx, uid, "wiki", []string{"workspace-knowledge", "rooms"}); err != nil {
		t.Fatal(err)
	}
	if got := rpc("wiki", list); !strings.Contains(got, `"rooms_free"`) || !strings.Contains(got, `"bikes_free"`) {
		t.Fatal(got)
	}
	// The unscoped discovery endpoint cannot expose the person's entire catalog.
	if got := rpc("", list); strings.Contains(got, `"rooms_free"`) {
		t.Fatal(got)
	}
	if err := st.SetOptIn(ctx, uid, "rooms", false); err != nil {
		t.Fatal(err)
	}
	if got := rpc("wiki", list); strings.Contains(got, `"rooms_free"`) {
		t.Fatal(got)
	}
}
