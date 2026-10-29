// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// This tests the admission barrier, not per-session runtime authorization.
func TestStagedAssistantRawGatewayBypass(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, err := st.EnsureUser(ctx, identity.Identity{UID: strings.Repeat("a", 64), Username: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AcceptConsent(ctx, u.UID, consent.Version(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProject(ctx, store.Project{ID: "p", UID: u.UID, Folder: "support", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	passed := 0
	h := gwGate(config.Config{UserNetPrefix: "10.90"}, st, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { passed++; w.WriteHeader(204) }))
	request := func(path, body string) int {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.RemoteAddr = "10.90.1.10:1234"
		r.Header.Set("X-OpenCode-Session-Id", "ses_forged1234")
		r.Header.Set("X-Profile-Id", "helper")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := request("/mcp/workspace", `{"method":"tools/list"}`); got != 204 {
		t.Fatalf("legacy access %d", got)
	}
	p, ok := instance.Current().Assistant("it-hilfe")
	if !ok {
		t.Fatal("no example")
	}
	if err := st.PinProjectAssistant(ctx, u.UID, "support", instance.Current().AssistantPin(p)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/mcp/workspace", "/mcp/wiki", "/v1/chat/completions"} {
		for _, body := range []string{`{"method":"tools/list"}`, `{"method":"tools/call","params":{"name":"web_read","_meta":{"ai.opencode/sessionID":"ses_other1234","profile":"helper"}}}`, `{"method":"tools/call","params":{"name":"knowledge_read","_meta":{"ai.opencode/sessionID":"ses_child1234"}}}`} {
			if got := request(path, body); got != http.StatusForbidden {
				t.Fatalf("bypass %s: %d", path, got)
			}
		}
	}
	if passed != 1 {
		t.Fatalf("upstream calls %d", passed)
	}
}
