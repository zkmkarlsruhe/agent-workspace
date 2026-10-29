// SPDX-License-Identifier: MIT

package central

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistantchat"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// A real private session drives the native MCP transport. The authorization
// callback comes from session/account/pin state, not a trusted-pin fixture.
func TestLiveAssistantSessionsScopeNativeMCP(t *testing.T) {
	ctx := context.Background()
	d, err := testinstance.Load("../../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	d.KnowledgeSources = assistants.Sources{
		"wiki":  {MCPServer: "workspace-knowledge/wiki", ReadOnly: true, Instruction: "Support wiki", Tools: []string{"lookup", "write"}},
		"works": {MCPServer: "workspace-knowledge/works", ReadOnly: true, Instruction: "Works catalogue", Tools: []string{"lookup"}},
	}
	d.Assistants[0].KnowledgeSources = []string{"wiki"}
	second := d.Assistants[0]
	second.ID = "works"
	second.KnowledgeSources = []string{"works"}
	// This fixture owns exactly the two assistants backed by its source registry.
	d.Assistants = []assistants.Definition{d.Assistants[0], second}
	defer instance.Select(d)()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	uid := strings.Repeat("a", 64)
	u, err := st.EnsureUser(ctx, identity.Identity{UID: uid, Username: "owner", Issuer: "https://id.example", Subject: "subject"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AcceptConsent(ctx, uid, consent.Version(), time.Now()); err != nil {
		t.Fatal(err)
	}
	upstream := sdk.NewServer(&sdk.Implementation{Name: "native", Version: "1"}, nil)
	calls := []string{}
	revoke := false
	personalOptOut := false
	granted := true
	for _, name := range []string{"wiki-lookup", "wiki-write", "works-lookup"} {
		upstream.AddTool(&sdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}, Annotations: &sdk.ToolAnnotations{ReadOnlyHint: !strings.HasSuffix(name, "write")}}, func(_ context.Context, r *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			calls = append(calls, r.Params.Name)
			if personalOptOut {
				if err := st.SetOptIn(ctx, uid, "workspace-knowledge", false); err != nil {
					t.Fatal(err)
				}
			}
			if revoke {
				granted = false
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "https://wiki.example/approved"}}}, nil
		})
	}
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return upstream }, nil)
	up, _ := url.Parse("http://native.test")
	central := New(Server{Upstream: up, APIKey: "manager-secret", Users: st, Usage: st, Log: slog.New(slog.DiscardHandler)})
	servers := []string{}
	central.http.Transport = handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer manager-secret" || r.Header.Get("x-litellm-end-user-id") != u.Username {
			t.Error("upstream identity missing")
		}
		servers = append(servers, r.Header.Get("x-mcp-servers"))
		handler.ServeHTTP(w, r)
	})}
	catalogDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(catalogDir, "mcp.json"), []byte(`{"items":[{"id":"workspace-knowledge","kind":"mcp-central","title":"Knowledge","description":"Wiki and works","tier":"optional","category":"research","litellm":["wiki","works"],"tools":["lookup","write"]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cat := &catalog.Service{Dir: catalogDir, Store: st, Log: slog.New(slog.DiscardHandler)}
	central.Items = func(ctx context.Context, uid string) []catalog.Item {
		return cat.OnItems(ctx, uid, catalog.KindCentral)
	}
	central.AssistantItems = func(p assistants.Definition) []catalog.Item {
		if !granted {
			return nil
		}
		return cat.AssistantItems(p)
	}
	central.SlotOf = func(netip.Addr) (int, bool) { return u.Slot, true }
	assertPersonalOff := func() {
		t.Helper()
		ids, err := st.OptIns(ctx, uid)
		if err != nil || len(ids) != 0 {
			t.Fatal("assistant changed personal catalog", ids, err)
		}
		if w := rpc(central, me, "workspace-knowledge", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); w.Code != http.StatusForbidden {
			t.Fatal("assistant source leaked to normal helper", w.Code, w.Body)
		}
	}
	assertPersonalOff()
	service := &assistantchat.Service{Store: st, Items: central.AssistantItems, Tools: central.AssistantTools, Call: central.AssistantCall}
	step := 0
	requested := "workspace-knowledge_lookup"
	service.Complete = func(_ context.Context, _ store.User, _, _ string, _ int, messages, tools any) (json.RawMessage, error) {
		step++
		b, _ := json.Marshal(tools)
		if strings.Contains(string(b), "write") {
			t.Fatal("write tool offered")
		}
		if step == 1 {
			return json.RawMessage(`{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"` + requested + `","arguments":"{}"}}]}`), nil
		}
		return json.RawMessage(`{"role":"assistant","content":"Follow https://wiki.example/approved"}`), nil
	}
	choices := service.Choices(ctx, uid, nil)
	if len(choices) != 2 {
		t.Fatal("fresh account assistants missing", choices)
	}
	for _, choice := range choices {
		if choice.Missing != "" {
			t.Fatal("fresh account card disabled", choice.Missing)
		}
	}
	for _, assistant := range []string{"it-hilfe", "works"} {
		def, _ := d.Assistant(assistant)
		folder, err := service.Create(ctx, uid, nil, assistant, d.AssistantPin(def).Hash, "Test native source lookup", false)
		if err != nil {
			t.Fatal(err)
		}
		id, err := service.Open(ctx, uid, nil, folder)
		if err != nil {
			t.Fatal(err)
		}
		step = 0
		servers = nil
		if err := service.Send(ctx, uid, nil, id, "please read guidance", 0); err != nil {
			t.Fatal(err)
		}
		assertPersonalOff()
		wanted := "wiki"
		if assistant == "works" {
			wanted = "works"
		}
		for _, got := range servers {
			if got != wanted {
				t.Fatalf("assistant %s got server %s", assistant, got)
			}
		}
		if got := calls[len(calls)-1]; got != wanted+"-lookup" {
			t.Fatalf("cross-assistant execution %s", got)
		}
		project, err := st.ProjectByFolder(ctx, uid, folder)
		if err != nil {
			t.Fatal(err)
		}
		usage, err := st.SessionUsage(ctx, uid, project.ID, time.Time{})
		if err != nil || len(usage) != 1 || usage[0].SessionID != id || usage[0].CostEUR <= 0 {
			t.Fatalf("native usage lost private session: %+v %v", usage, err)
		}
		if _, err := st.ChatByID(ctx, id); err != store.ErrNotFound {
			t.Fatal("operator read", err)
		}
		// A fabricated subagent/builtin request cannot get a second executor.
		step = 0
		requested = "subagent"
		before := len(calls)
		if err := service.Send(ctx, uid, nil, id, "delegate this task", 1); err == nil || len(calls) != before {
			t.Fatal("delegation escaped assistant", err)
		}
		// A personal opt-out during a call does not revoke the assistant grant.
		if err := st.SetOptIn(ctx, uid, "workspace-knowledge", true); err != nil {
			t.Fatal(err)
		}
		personalOptOut = true
		result, err := central.AssistantCall(ctx, u, id, "workspace-knowledge_lookup", json.RawMessage(`{}`), func() (assistants.Pin, error) {
			current, err := service.Authorize(ctx, uid, nil, assistant, d.AssistantPin(def).Hash)
			return d.AssistantPin(current), err
		})
		if err != nil || result.IsError {
			t.Fatal("personal opt-out revoked assistant", result, err)
		}
		personalOptOut = false
		assertPersonalOff()
		requested = "workspace-knowledge_lookup"
		// Revoke while an allowed native call is in flight; its result is discarded.
		revoke = true
		step = 0
		if err := service.Send(ctx, uid, nil, id, "lookup after revocation", 1); err == nil {
			t.Fatal("revoked result accepted")
		}
		se, err := st.AssistantSession(ctx, uid, id)
		if err != nil || se.Revision != 1 {
			t.Fatal("revoked result committed", se, err)
		}
		revoke = false
		granted = true
	}
	// The OpenCode project endpoint uses the same grants; normal helper calls
	// and missing/foreign session bindings cannot use it.
	pin := d.AssistantPin(d.Assistants[0])
	central.AssistantSession = func(_ context.Context, gotUID, project, session string) (assistants.Pin, error) {
		if gotUID != uid || project != "bound-project" || (session != "" && session != "ses_bound1234") {
			return assistants.Pin{}, errors.New("unbound session")
		}
		return pin, nil
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp/assistant/{project}/{name}", central)
	mux.Handle("/mcp/profile/{project}/{name}", central)
	endpoint := "/mcp/assistant/"
	assistantRPC := func(project, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", endpoint+project+"/workspace-knowledge", strings.NewReader(body))
		r.RemoteAddr = me
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	w := assistantRPC("bound-project", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"lookup"`) || strings.Contains(w.Body.String(), `"name":"write"`) {
		t.Fatal("fresh assistant discovery failed", w.Code, w.Body)
	}
	for _, session := range []string{"", "ses_helper1234", "ses_other1234", "ses_bound1234"} {
		before := len(calls)
		w := assistantRPC("bound-project", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","_meta":{"ai.opencode/sessionID":"`+session+`"}}}`)
		if session == "ses_bound1234" {
			if len(calls) != before+1 || !strings.Contains(w.Body.String(), "https://wiki.example/approved") {
				t.Fatal("assistant call denied", w.Body)
			}
		} else if len(calls) != before || !strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatal("unbound call granted", session, w.Body)
		}
	}
	if w := assistantRPC("other-project", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); w.Code != 403 {
		t.Fatal("foreign project granted", w.Body)
	}
	endpoint = "/mcp/profile/"
	if w := assistantRPC("bound-project", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"lookup"`) {
		t.Fatal("legacy project endpoint lost assistant grants", w.Code, w.Body)
	}
	if w := assistantRPC("other-project", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); w.Code != 403 {
		t.Fatal("legacy project endpoint accepted a foreign project", w.Body)
	}
	assertPersonalOff()
	// Client headers cannot invoke the internal assistant path.
	central.SlotOf = func(addr netip.Addr) (int, bool) { return u.Slot, true }
	central.Assistant = func(ctx context.Context, uid string) (assistants.Pin, error) {
		return assistants.Pin{}, st.SharedRuntimeAccess(ctx, uid)
	}
	r := httptest.NewRequest("POST", "http://gateway/mcp/workspace-knowledge", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","_meta":{"ai.opencode/sessionID":"ses_profile_forged","profile":"works"}}}`))
	r.RemoteAddr = "10.90.1.10:1000"
	r.SetPathValue("name", "workspace-knowledge")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w = httptest.NewRecorder()
	central.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("raw runtime claimed assistant context: %d %s", w.Code, w.Body)
	}
}

func TestAssistantExactToolRevocation(t *testing.T) {
	for _, phase := range []string{"listing", "result"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			d, err := testinstance.Load("../../../../instance/example/instance.yaml")
			if err != nil {
				t.Fatal(err)
			}
			d.KnowledgeSources = assistants.Sources{"wiki": {MCPServer: "workspace-knowledge/wiki", ReadOnly: true, Instruction: "Wiki", Tools: []string{"lookup", "other"}}}
			d.Assistants[0].KnowledgeSources = []string{"wiki"}
			defer instance.Select(d)()
			granted := true
			calls := 0
			srv := sdk.NewServer(&sdk.Implementation{Name: "native", Version: "1"}, nil)
			srv.AddTool(&sdk.Tool{Name: "wiki-lookup", InputSchema: map[string]any{"type": "object"}, Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				calls++
				if phase == "result" {
					granted = false
				}
				return &sdk.CallToolResult{}, nil
			})
			srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
				return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
					res, err := next(ctx, method, req)
					if method == "tools/list" && phase == "listing" {
						granted = false
					}
					return res, err
				}
			})
			handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
			up, _ := url.Parse("http://native.test")
			s := New(Server{Upstream: up, Usage: &rec{}, Log: slog.New(slog.DiscardHandler)})
			s.http.Transport = handlerTransport{handler}
			s.AssistantItems = func(assistants.Definition) []catalog.Item {
				tools := []string{"other"}
				if granted {
					tools = append(tools, "lookup")
				}
				return []catalog.Item{{ID: "workspace-knowledge", Kind: catalog.KindCentral, LiteLLM: []string{"wiki"}, Tools: tools}}
			}
			_, err = s.AssistantCall(ctx, store.User{UID: "u1"}, "ses_profile_test", "workspace-knowledge_lookup", json.RawMessage(`{}`), func() (assistants.Pin, error) { return d.AssistantPin(d.Assistants[0]), nil })
			if err == nil {
				t.Fatal("revoked tool result accepted")
			}
			want := 0
			if phase == "result" {
				want = 1
			}
			if calls != want {
				t.Fatal("revoked tool dispatched", calls)
			}
		})
	}
}
