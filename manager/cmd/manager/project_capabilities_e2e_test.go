// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/mcp"
)

// Portal creation, persistence, location config, MCP enforcement and deferred reload together.
func TestProjectCapabilitiesE2E(t *testing.T) {
	s := newStack(t, "127.89")
	s.cat.Projects, s.cat.SessionDir = s.st, s.svc.SessionDir
	s.svc.PrepareCapabilities = s.cat.Prepare
	// Keep every generated runtime file inside the harness workspace.
	s.rt.Paths["/home/user/.config/opencode/catalog.json"] = filepath.Join(t.TempDir(), "catalog.json")
	ctx := context.Background()
	s.do(t, call{url: "/tools", as: &alice})
	for _, id := range []string{"rooms", "workspace-knowledge"} {
		if err := s.st.SetOptIn(ctx, alice.uid, id, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.svc.EnsureRunning(ctx, alice.uid); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("description", "Bitte recherchiere nur im Wiki.")
	mw.WriteField("capabilities_present", "1")
	mw.WriteField("capability", "workspace-knowledge")
	mw.Close()
	wantLoc(t, s.do(t, call{method: "POST", url: "/projects/start", body: &body, ctype: mw.FormDataContentType(), as: &alice}), "https://alice.apps.example.org/server/")
	p, err := s.st.ProjectByFolder(ctx, alice.uid, "besucher-auswertung")
	if err != nil || !slices.Equal(p.Capabilities, []string{"workspace-knowledge"}) {
		t.Fatal(p, err)
	}
	b, err := os.ReadFile(filepath.Join(s.ws, p.Folder, "opencode.json"))
	if err != nil || !strings.Contains(string(b), "/mcp/project/"+p.ID+"/workspace") {
		t.Fatal(string(b), err)
	}
	// The runtime fixture uses short session IDs; use one accepted by the real MCP boundary.
	s.oc.Mu.Lock()
	s.oc.Sessions[0].ID = "ses_project1234"
	s.oc.Mu.Unlock()
	gateway := mcp.New(mcp.Server{Users: s.st, Usage: s.st, Rooms: &mcp.Rooms{}, Log: slog.New(slog.DiscardHandler),
		SlotOf: func(netip.Addr) (int, bool) { return 1, true }, Grants: s.cat.Grants, ProjectAccess: s.cat.ProjectAccess, ProjectScoped: true})
	rpc := func(body string) string {
		req := httptest.NewRequest("POST", "/mcp/project/"+p.ID+"/workspace", strings.NewReader(body))
		req.SetPathValue("capProject", p.ID)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	if got := rpc(list); strings.Contains(got, `"rooms_free"`) {
		t.Fatal(got)
	}
	got := rpc(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"rooms_free","arguments":{},"_meta":{"ai.opencode/sessionID":"ses_project1234"}}}`)
	if !strings.Contains(got, `"isError":true`) {
		t.Fatal("direct call accepted", got)
	}
	s.oc.Mu.Lock()
	s.oc.Active = []string{"ses_project1234"}
	reloads := s.oc.Reloads
	s.oc.Mu.Unlock()
	wantLoc(t, s.do(t, call{method: "POST", url: "/projects/" + p.Folder + "/capabilities", form: url.Values{"capability": {"rooms"}, "on": {"1"}}, as: &alice}), fmt.Sprintf("/projects/%s?effect=restart", p.Folder))
	if got := rpc(list); !strings.Contains(got, `"rooms_free"`) {
		t.Fatal(got)
	}
	s.oc.Mu.Lock()
	if s.oc.Reloads != reloads {
		t.Error("reloaded while busy")
	}
	s.oc.Active = nil
	s.oc.Mu.Unlock()
	s.cat.RetryPending(ctx)
	s.oc.Mu.Lock()
	defer s.oc.Mu.Unlock()
	if s.oc.Reloads != reloads+1 {
		t.Fatal("reload not retried", s.oc.Reloads)
	}
}
