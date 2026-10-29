// SPDX-License-Identifier: MIT

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
	"log/slog"
	"net/http/httptest"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
	"testing"
)

func TestRawAssistantContextDenied(t *testing.T) {
	// No executor or upstream is configured: rejection precedes tool dispatch.
	s := &Server{}
	for _, name := range []string{"web_search", "toolbox_run", "feedback_send", "nextcloud_upload"} {
		result, err := s.callTool(context.Background(), store.User{UID: "owner"}, &sdk.CallToolParamsRaw{Name: name, Arguments: json.RawMessage(`{}`), Meta: sdk.Meta{"ai.opencode/sessionID": "ses_profile_forged"}})
		if err != nil || !result.IsError {
			t.Fatalf("raw profile context %s: %v %+v", name, err, result)
		}
	}
}

func TestAssistantOrganisationGrants(t *testing.T) {
	d, err := testinstance.Load("../../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Select(d)()
	def, _ := d.Assistant("research")
	pin := d.AssistantPin(def)
	valid := true
	r := &rec{}
	s := New(Server{SearXNG: mustURL("https://search.example"), SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf,
		Users: users{}, Usage: r, Log: slog.New(slog.DiscardHandler),
		ProjectScoped: true,
		ProjectAccess: func(context.Context, string) bool { return false },
		Grants:        func(context.Context, string, string) (map[string]bool, map[string]string) { return nil, nil },
		AssistantSession: func(_ context.Context, uid, project, session string) (assistants.Pin, error) {
			if !valid || uid != "u1" || project != "owned" || (session != "" && session != "ses_research123") {
				return assistants.Pin{}, errors.New("denied")
			}
			return pin, nil
		}})
	request := func(project, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/mcp/assistant/"+project+"/workspace", strings.NewReader(body))
		req.SetPathValue("project", project)
		req.RemoteAddr = "10.90.1.10:40000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	w := request("owned", list)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"web_search"`) || !strings.Contains(w.Body.String(), `"web_read"`) || strings.Contains(w.Body.String(), `"feedback_send"`) {
		t.Fatal(w.Code, w.Body)
	}
	if w := request("", list); strings.Contains(w.Body.String(), `"web_search"`) {
		t.Fatal("grant leaked to personal endpoint", w.Body)
	}
	if w := request("other", list); w.Code != 403 {
		t.Fatal("foreign project allowed", w.Code)
	}
	// Invalid arguments exercise dispatch and metering without making a network request.
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"web_search","arguments":{},"_meta":{"ai.opencode/sessionID":"ses_research123"}}}`
	for _, session := range []string{"", "ses_wrong123", "ses_profile_forged"} {
		request("owned", strings.Replace(call, "ses_research123", session, 1))
		if len(r.ev) != 0 {
			t.Fatal("invalid session reached tool", session)
		}
	}
	request("owned", strings.Replace(call, "web_search", "feedback_send", 1))
	if len(r.ev) != 0 {
		t.Fatal("ungranted tool dispatched")
	}
	request("owned", call)
	if len(r.ev) != 1 {
		t.Fatal("bound web call did not reach tool", r.ev)
	}
	d.Integrations["web-research"] = false
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if w := request("owned", list); strings.Contains(w.Body.String(), `"web_search"`) {
		t.Fatal("integration off ignored", w.Body)
	}
	request("owned", call)
	if len(r.ev) != 1 {
		t.Fatal("disabled integration dispatched")
	}
	valid = false
	if w := request("owned", list); w.Code != 403 {
		t.Fatal("revoked binding allowed", w.Code)
	}
}
