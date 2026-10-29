// SPDX-License-Identifier: MIT

package central

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Reuse mcp-hello's in-memory protocol setup behind a broker-shaped HTTP handler.
// This tests the manager contract, not the deployed LiteLLM implementation.
func TestHelloBrokerOverlay(t *testing.T) {
	ctx := context.Background()
	d, err := instance.LoadWithIntegrations("../../../../instance/minimal/instance.yaml", "../../../../deploy/example/mcp-hello/integrations")
	if err != nil {
		t.Fatal(err)
	}
	d.Integrations["hello"] = true
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(instance.Select(d))
	items, err := catalog.Load("../../../../deploy/example/mcp-hello")
	if err != nil || len(items) != 1 {
		t.Fatalf("catalog: %v %v", items, err)
	}
	source := assistants.Source{MCPServer: "hello/hello", ReadOnly: true, Instruction: "Read the greeting and cite its source.", Tools: []string{"hello"}}
	if err := ValidateSources(assistants.Sources{"greeting": source}, items); err != nil {
		t.Fatal(err)
	}
	hello := sdk.NewServer(&sdk.Implementation{Name: "hello", Version: "0.1.0"}, nil)
	calls := 0
	hello.AddTool(&sdk.Tool{Name: "hello", InputSchema: map[string]any{"type": "object"}, Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		calls++
		if req.Params.Name != "hello" {
			t.Errorf("non-native backend name: %s", req.Params.Name)
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "Hello from the example knowledge source. Source: https://example.org/hello"}}}, nil
	})
	hello.AddTool(&sdk.Tool{Name: "unlisted", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		t.Error("unlisted tool called")
		return &sdk.CallToolResult{}, nil
	})
	ct, st := sdk.NewInMemoryTransports()
	ss, err := hello.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "broker-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	broker := sdk.NewServer(&sdk.Implementation{Name: "broker-test", Version: "1"}, &sdk.ServerOptions{Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}}})
	var upstreamNames []string
	broker.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			switch method {
			case "tools/list":
				list, err := cs.ListTools(ctx, nil)
				if err != nil {
					return nil, err
				}
				for _, tool := range list.Tools {
					tool.Name = "hello-" + tool.Name
				}
				return list, nil
			case "tools/call":
				p := req.(*sdk.CallToolRequest).Params
				upstreamNames = append(upstreamNames, p.Name)
				return cs.CallTool(ctx, &sdk.CallToolParams{Name: strings.TrimPrefix(p.Name, "hello-"), Arguments: p.Arguments})
			}
			return next(ctx, method, req)
		}
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return broker }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	endpoint, _ := url.Parse("http://broker.test")
	s := New(Server{Upstream: endpoint, APIKey: "manager-key", Users: users{}, Usage: &rec{}, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Log: slog.New(slog.DiscardHandler), Items: func(context.Context, string) []catalog.Item { return items }})
	requests := 0
	s.http.Transport = handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/mcp/" || r.Header.Get("x-mcp-servers") != "hello" || r.Header.Get("Authorization") != "Bearer manager-key" || r.Header.Get("x-litellm-end-user-id") != "alice" {
			t.Errorf("broker request: %s %v", r.URL, r.Header)
		}
		handler.ServeHTTP(w, r)
	})}
	w := rpc(s, me, "hello", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var listing struct{ Result sdk.ListToolsResult }
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil || len(listing.Result.Tools) != 1 || listing.Result.Tools[0].Name != "hello" || !listing.Result.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("discovery: %s %v", w.Body, err)
	}
	w = rpc(s, me, "hello", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hello","arguments":{}}}`)
	if !strings.Contains(w.Body.String(), "https://example.org/hello") || calls != 1 || len(upstreamNames) != 1 || upstreamNames[0] != "hello-hello" {
		t.Fatalf("native call: %s %v %d", w.Body, upstreamNames, calls)
	}
	before := requests
	for _, name := range []string{"unlisted", "hello-hello"} {
		w = rpc(s, me, "hello", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+name+`","arguments":{}}}`)
		if !strings.Contains(w.Body.String(), `"isError":true`) || requests != before {
			t.Fatalf("denial reached upstream: %s", w.Body)
		}
	}
	// Assistant source scopes retain native allowlists and annotation checks.
	tools, err := s.listTools(ctx, store.User{Username: "alice"}, items[0], []assistants.Source{source})
	if err != nil || len(tools.Tools) != 1 {
		t.Fatalf("read-only source: %v %v", tools, err)
	}
	hello.AddTool(&sdk.Tool{Name: "hello", InputSchema: map[string]any{"type": "object"}, Annotations: &sdk.ToolAnnotations{ReadOnlyHint: false}}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		t.Error("write tool called")
		return nil, nil
	})
	tools, err = s.listTools(ctx, store.User{Username: "alice"}, items[0], []assistants.Source{source})
	if err != nil || len(tools.Tools) != 0 {
		t.Fatalf("write annotation accepted: %v %v", tools, err)
	}
	d.Integrations["hello"] = false
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	before = requests
	w = rpc(s, me, "hello", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"hello"}}`)
	if w.Code != http.StatusForbidden || requests != before {
		t.Fatalf("revocation: %d %s", w.Code, w.Body)
	}
}
