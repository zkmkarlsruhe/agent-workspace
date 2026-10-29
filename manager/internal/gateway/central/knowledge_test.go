// SPDX-License-Identifier: MIT

package central

import (
	"context"
	"encoding/json"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/testinstance"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func TestRegistryCatalogReferences(t *testing.T) {
	items, err := catalog.Load("../../../../deploy/catalog")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSources(instance.Current().KnowledgeSources, items); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"missing/outline", "workspace-knowledge/outline", "rooms/workspace_kb_wiki"} {
		r := assistants.Sources{"wiki": {MCPServer: ref, ReadOnly: true, Instruction: "Use for support."}}
		if ValidateSources(r, items) == nil {
			t.Fatalf("unknown MCP accepted: %s", ref)
		}
	}
	r := assistants.Sources{"wiki": {MCPServer: "workspace-knowledge/workspace_kb_wiki", ReadOnly: true, Instruction: "Use for support.", Tools: []string{"ungranted"}}}
	if ValidateSources(r, items) == nil {
		t.Fatal("tool outside catalog accepted")
	}
}

func TestAssistantKnowledgeGateway(t *testing.T) {
	d, err := testinstance.Load("../../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Native names deliberately do not resemble search/read verbs.
	d.KnowledgeSources = assistants.Sources{
		"wiki":  {MCPServer: "workspace-knowledge/outline", ReadOnly: true, Instruction: "Use wiki for support.", Tools: []string{"lookup.v2", "mutate", "legacy", "contradictory"}},
		"works": {MCPServer: "workspace-knowledge/works", ReadOnly: true, Instruction: "Use for art."},
	}
	d.Assistants[0].KnowledgeSources = []string{"wiki"}
	defer instance.Select(d)()
	pin := d.AssistantPin(d.Assistants[0])
	var calls []string
	destructive := true
	upstream := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	for _, c := range []struct {
		name        string
		annotations *sdk.ToolAnnotations
	}{
		{"outline-lookup.v2", &sdk.ToolAnnotations{ReadOnlyHint: true}},
		{"outline-mutate", &sdk.ToolAnnotations{ReadOnlyHint: false}},
		{"outline-legacy", nil},
		{"outline-contradictory", &sdk.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &destructive}},
		{"outline-added", &sdk.ToolAnnotations{ReadOnlyHint: true}},
		{"works-lookup.v2", &sdk.ToolAnnotations{ReadOnlyHint: true}},
	} {
		upstream.AddTool(&sdk.Tool{Name: c.name, Annotations: c.annotations, InputSchema: map[string]any{"type": "object"}},
			func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				calls = append(calls, req.Params.Name)
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "https://wiki.example/page"}}}, nil
			})
	}
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return upstream }, nil)
	var servers string
	up, _ := url.Parse("http://mcp.test")
	s := New(Server{Upstream: up, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf,
		Users: users{}, Usage: &rec{}, Log: slog.New(slog.DiscardHandler)})
	s.http.Transport = handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servers = r.Header.Get("x-mcp-servers")
		h.ServeHTTP(w, r)
	})}
	s.Items = func(context.Context, string) []catalog.Item {
		return []catalog.Item{{ID: "workspace-knowledge", Kind: catalog.KindCentral, LiteLLM: []string{"outline", "works"}, Tools: []string{"lookup.v2", "mutate", "legacy", "added", "contradictory"}},
			{ID: "library", Kind: catalog.KindCentral, LiteLLM: []string{"opac"}, Tools: []string{"lookup.v2"}}}
	}
	s.Assistant = func(context.Context, string) (assistants.Pin, error) { return pin, nil }
	list := func() []string {
		t.Helper()
		w := rpc(s, me, "workspace-knowledge", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		var res struct {
			Result struct{ Tools []struct{ Name string } }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatal(err, w.Body)
		}
		var names []string
		for _, tool := range res.Result.Tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		return names
	}
	if got := list(); !slices.Equal(got, []string{"legacy", "lookup.v2"}) {
		t.Fatalf("tools %v", got)
	}
	if servers != "outline" {
		t.Fatalf("server scope %s", servers)
	}
	for _, name := range []string{"mutate", "added", "works-lookup.v2", "contradictory"} {
		w := rpc(s, me, "workspace-knowledge", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+name+`","_meta":{"profile":"other","ai.opencode/sessionID":"ses_forged1234"}}}`)
		if !strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatalf("allowed %s: %s", name, w.Body)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("write or out-of-scope calls reached upstream: %v", calls)
	}
	w := rpc(s, me, "workspace-knowledge", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"lookup.v2"}}`)
	if !strings.Contains(w.Body.String(), "https://wiki.example/page") || !slices.Equal(calls, []string{"outline-lookup.v2"}) {
		t.Fatalf("read failed: %s %v", w.Body, calls)
	}
	grantedItems := s.Items
	s.Items = func(context.Context, string) []catalog.Item { return nil }
	if w := rpc(s, me, "workspace-knowledge", `{"jsonrpc":"2.0","id":8,"method":"tools/list"}`); w.Code != 403 {
		t.Fatal("profile granted a disabled catalog entry")
	}
	s.Items = grantedItems
	// Re-list at call time: a changed tool must not retain its earlier read grant.
	upstream.AddTool(&sdk.Tool{Name: "outline-lookup.v2", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false}, InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			t.Error("changed write tool reached upstream")
			return &sdk.CallToolResult{}, nil
		})
	w = rpc(s, me, "workspace-knowledge", `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"lookup.v2"}}`)
	if !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("stale read annotation accepted")
	}
	if w := rpc(s, me, "library", `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`); w.Code != 403 {
		t.Fatalf("unselected server status %d", w.Code)
	}
	// Optional allowlist requires explicit read annotations; the other profile
	// sees only its own server, even where tool names collide.
	d.Assistants[0].KnowledgeSources = []string{"works"}
	pin = d.AssistantPin(d.Assistants[0])
	if got := list(); !slices.Equal(got, []string{"lookup.v2"}) || servers != "works" {
		t.Fatalf("other profile tools %v, servers %s", got, servers)
	}
	d.KnowledgeSources["works"] = assistants.Source{MCPServer: "workspace-knowledge/works", ReadOnly: true, Instruction: "Changed scope."}
	if w := rpc(s, me, "workspace-knowledge", `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`); w.Code != 403 {
		t.Fatal("stale pin accepted")
	}
	s.Assistant = func(context.Context, string) (assistants.Pin, error) {
		return assistants.Pin{}, assistants.ErrIsolationRequired
	}
	if w := rpc(s, me, "workspace-knowledge", `{"jsonrpc":"2.0","id":6,"method":"tools/list"}`); w.Code != 403 {
		t.Fatal("untrusted runtime admitted")
	}
}

// Exercise the actual MCP transport without requiring local TCP sockets.
type handlerTransport struct{ http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result(), nil
}
