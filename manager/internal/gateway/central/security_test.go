// SPDX-License-Identifier: MIT

package central

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestBrokerRefusesRedirects(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		for _, target := range []string{"https://other.test/mcp/", "http://broker.test/mcp/", "https://broker.test:8443/mcp/", "https://broker.test/other"} {
			t.Run(fmt.Sprintf("%d/%s", code, target), func(t *testing.T) {
				requests := 0
				client := &http.Client{Transport: handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					if requests > 1 {
						t.Error("followed redirect")
						w.WriteHeader(500)
						return
					}
					if r.Header.Get("Authorization") != "Bearer secret" {
						t.Error("missing configured credentials")
					}
					w.Header().Set("Location", target)
					w.WriteHeader(code)
				})}}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				cs, err := (litellmBroker{}).connect(ctx, "https://broker.test/mcp/", "secret", "person", []string{"srv"}, client)
				if cs != nil {
					cs.Close()
				}
				if err == nil || requests != 1 {
					t.Fatalf("requests=%d err=%v", requests, err)
				}
			})
		}
	}
}

func TestUpstreamOrigin(t *testing.T) {
	origin, _ := url.Parse("https://broker.test:443/mcp/")
	for _, target := range []string{"https://broker.test:443/other", "http://broker.test:443/mcp/", "https://other.test:443/mcp/", "https://broker.test:8443/mcp/"} {
		sent := false
		transport := upstreamRT{origin: origin, hdr: http.Header{"Authorization": []string{"Bearer secret"}, "X-Mcp-Servers": []string{"srv"}}, base: handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sent = true
			if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("x-mcp-servers") != "srv" {
				t.Fatal("missing headers")
			}
			w.WriteHeader(200)
		})}}
		req, _ := http.NewRequest("POST", target, nil)
		resp, err := transport.RoundTrip(req)
		if resp != nil {
			resp.Body.Close()
		}
		allowed := target == "https://broker.test:443/other"
		if sent != allowed || (err == nil) != allowed {
			t.Fatalf("target=%s sent=%v err=%v", target, sent, err)
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("mutated original request")
		}
	}
}

func TestBrokerKeylessHeaders(t *testing.T) {
	for _, key := range []string{"", "secret"} {
		broker := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, nil)
		h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return broker }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
		requests := 0
		client := &http.Client{Transport: handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if key == "" {
				if _, exists := r.Header["Authorization"]; exists {
					t.Errorf("keyless Authorization present: %v", r.Header)
				}
			} else if r.Header.Get("Authorization") != "Bearer "+key {
				t.Errorf("auth=%v", r.Header)
			}
			if r.Header.Get("x-mcp-servers") != "srv" || r.Header.Get("x-litellm-end-user-id") != "person" {
				t.Error("lost broker attribution")
			}
			h.ServeHTTP(w, r)
		})}}
		cs, err := (litellmBroker{}).connect(context.Background(), "https://broker.test/mcp/", key, "person", []string{"srv"}, client)
		if err != nil {
			t.Fatal(err)
		}
		cs.Close()
		if requests == 0 {
			t.Fatal("no requests")
		}
	}
}

func TestToolsPagination(t *testing.T) {
	for _, mode := range []string{"two-pages", "repeat", "limit", "twenty-pages"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			var called string
			broker := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "1"}, &sdk.ServerOptions{Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}}})
			broker.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
				return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
					switch method {
					case "tools/list":
						requests++
						cursor := req.(*sdk.ListToolsRequest).Params.Cursor
						if requests == 1 && cursor != "" {
							t.Errorf("first cursor=%q", cursor)
						}
						result := &sdk.ListToolsResult{NextCursor: fmt.Sprintf("page-%d", requests+1)}
						names := []string{"srv-first", "srv-unlisted", "other-denied", "srv-write"}
						if requests > 1 {
							names = []string{"srv-first", "srv-lookup"}
						}
						for _, name := range names {
							result.Tools = append(result.Tools, &sdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}, Annotations: &sdk.ToolAnnotations{ReadOnlyHint: name != "srv-write"}})
						}
						switch mode {
						case "two-pages":
							if requests == 2 {
								result.NextCursor = ""
							}
						case "repeat":
							result.NextCursor = "repeat"
						case "twenty-pages":
							if requests == 20 {
								result.NextCursor = ""
							}
						}
						return result, nil
					case "tools/call":
						called = req.(*sdk.CallToolRequest).Params.Name
						return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "found"}}}, nil
					}
					return next(ctx, method, req)
				}
			})
			h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return broker }, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
			endpoint, _ := url.Parse("https://broker.test")
			s := New(Server{Upstream: endpoint, Log: slog.New(slog.DiscardHandler)})
			s.http.Transport = handlerTransport{h}
			item := catalog.Item{ID: "source", LiteLLM: []string{"srv"}, Tools: []string{"first", "lookup", "denied", "write"}}
			scope := []assistants.Source{{MCPServer: "source/srv", ReadOnly: true, Tools: []string{"first", "lookup", "write"}}}
			cs, err := s.connect(context.Background(), store.User{}, item)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			got, err := s.tools(context.Background(), cs, item, scope)
			wantRequests := 2
			if mode == "limit" || mode == "twenty-pages" {
				wantRequests = 20
			}
			if requests != wantRequests {
				t.Fatalf("requests=%d want %d", requests, wantRequests)
			}
			if mode == "repeat" || mode == "limit" {
				if err == nil || len(got) != 0 {
					t.Fatalf("pagination accepted: %v %v", got, err)
				}
				if mode == "repeat" && !strings.Contains(err.Error(), "repeated cursor") {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tool := range got {
				names = append(names, tool.def.Name)
			}
			if !slices.Equal(names, []string{"first", "lookup"}) {
				t.Fatalf("tools=%v", names)
			}
			requests = 0
			result, status := s.call(context.Background(), store.User{}, item, scope, "lookup", nil)
			if status != 200 || result.IsError || called != "srv-lookup" {
				t.Fatalf("page-two call: %d %v %q", status, result, called)
			}
		})
	}
}
