// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type providerTransport func(*http.Request) (*http.Response, error)

func (f providerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the real proxy and logging transport without binding a listening socket.
func providerGateway(flavor, key string, handler http.HandlerFunc) (*Gateway, *rec) {
	u, _ := url.Parse("http://provider.example/prefix")
	a := instance.Current().Accounting
	a.UpstreamCostCurrency = "EUR"
	rec := &rec{}
	cfg := config.Config{UserNetPrefix: "10.90"}
	g := New(Gateway{Flavor: flavor, APIKey: key, Accounting: &a, Upstream: u, SlotOf: cfg.SlotOf,
		Users: users{}, Usage: rec, Log: slog.New(slog.DiscardHandler)})
	transport := g.proxy.Transport.(logx.Transport)
	transport.Next = providerTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler(w, r)
		resp := w.Result()
		resp.Request = r
		return resp, nil
	})
	g.proxy.Transport = transport
	return g, rec
}

func TestProviderHeadersAndInternalCalls(t *testing.T) {
	for _, flavor := range []string{"litellm", "openai"} {
		for _, key := range []string{"provider-key", ""} {
			t.Run(flavor+"/"+key, func(t *testing.T) {
				var headers []http.Header
				var paths []string
				g, rec := providerGateway(flavor, key, func(w http.ResponseWriter, r *http.Request) {
					headers = append(headers, r.Header.Clone())
					paths = append(paths, r.URL.Path)
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("x-litellm-model-id", "deployment")
					w.Header().Set("x-litellm-response-cost", "0.7")
					if strings.Contains(r.URL.Path, "/images/") {
						if strings.HasSuffix(r.URL.Path, "/edits") {
							if r.FormValue("user") != "" || r.FormValue("response_format") != "b64_json" {
								t.Error("invalid image edit fields")
							}
						} else {
							var body map[string]any
							json.NewDecoder(r.Body).Decode(&body)
							if body["user"] != nil || body["response_format"] != "b64_json" {
								t.Error("invalid image fields")
							}
						}
						io.WriteString(w, `{"data":[{"b64_json":"cG5n"}]}`)
					} else {
						var body map[string]any
						json.NewDecoder(r.Body).Decode(&body)
						if body["user"] != nil || body["model"] != instance.Current().Models.Small {
							t.Errorf("internal role mapping: %v", body)
						}
						for k := range body {
							switch k {
							case "model", "messages", "max_tokens", "tools":
							default:
								t.Errorf("unexpected internal field: %s", k)
							}
						}
						if len(headers) == 2 && (body["tools"] == nil || body["max_tokens"] != float64(25)) {
							t.Errorf("internal tools request: %v", body)
						}
						io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"folder-name","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
					}
				})
				ctx := logx.Set(context.Background(), logx.Fields{ReqID: "local-request", UID: "u1"})
				u := store.User{UID: "u1", Username: "private-username"}
				if result, err := g.Suggest(ctx, u, "describe a folder"); err != nil || result != "folder-name" {
					t.Fatalf("small-role naming: %q %v", result, err)
				}
				tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}
				if raw, err := g.AssistantCompletion(ctx, u, "ses_abcdefgh", instance.Current().Models.Small, 25,
					[]map[string]string{{"role": "user", "content": "look up"}}, tools); err != nil || !strings.Contains(string(raw), "tool_calls") {
					t.Fatalf("internal tools: %s %v", raw, err)
				}
				for _, src := range [][]byte{nil, []byte("png")} {
					if img, _, err := g.Image(ctx, u, "ses_abcdefgh", []string{"image-model"}, "a line", "1024x1024", src); err != nil || string(img) != "png" {
						t.Fatalf("image: %s %v", img, err)
					}
				}
				if len(headers) != 4 || len(rec.ev) != 4 || paths[0] != "/prefix/v1/chat/completions" || paths[2] != "/prefix/v1/images/generations" || paths[3] != "/prefix/v1/images/edits" {
					t.Fatalf("calls=%v events=%+v", paths, rec.ev)
				}
				for i, h := range headers {
					wantAuth := ""
					if key != "" {
						wantAuth = "Bearer " + key
					}
					if h.Get("Authorization") != wantAuth {
						t.Errorf("auth: %v", h)
					}
					e := rec.ev[i]
					if e.UID != "u1" || (i > 0 && e.SessionID != "ses_abcdefgh") {
						t.Errorf("local ownership: %+v", e)
					}
					if flavor == "openai" {
						for name, values := range h {
							if strings.HasPrefix(strings.ToLower(name), "x-litellm-") || strings.Contains(strings.Join(values, " "), u.Username) {
								t.Errorf("identity leaked: %v", h)
							}
						}
						if h.Get("X-Request-Id") != "" || e.DeploymentID != "" || e.RawCost != nil || e.CostSource != "estimate" {
							t.Errorf("direct attribution/cost: %v %+v", h, e)
						}
					} else if h.Get("x-litellm-end-user-id") != u.Username || h.Get("x-litellm-call-id") != e.EventID ||
						h.Get("x-litellm-spend-logs-metadata") == "" || (i > 0 && h.Get("x-litellm-session-id") != "ses_abcdefgh") ||
						e.DeploymentID != "deployment" || e.CostSource != "header" {
						t.Errorf("LiteLLM attribution/cost: %v %+v", h, e)
					}
				}
			})
		}
	}
}

func TestProviderSanitizerAndStreaming(t *testing.T) {
	for _, flavor := range []string{"litellm", "openai"} {
		t.Run(flavor, func(t *testing.T) {
			var got map[string]any
			var headers http.Header
			g, rec := providerGateway(flavor, "key", func(w http.ResponseWriter, r *http.Request) {
				headers = r.Header.Clone()
				json.NewDecoder(r.Body).Decode(&got)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
				io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
			})
			body := map[string]any{"model": instance.Current().Models.Default, "messages": []any{}, "stream": true, "max_tokens": 99999,
				"tools":          []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}},
				"stream_options": map[string]any{"include_usage": false}}
			for _, k := range blockedRequestKeys {
				body[k] = "untrusted"
			}
			body["n"] = 7
			raw, _ := json.Marshal(body)
			w := call(g, "POST", "/v1/chat/completions", "10.90.1.10:1234", string(raw), map[string]string{
				"Authorization": "client-secret", "x-litellm-end-user-id": "spoofed", "x-opencode-session-id": "ses_abcdefgh",
				"X-OpenWebUI-User-Email": "private@example.org", "x-opencode-project": "project"})
			if w.Code != 200 || !strings.Contains(w.Body.String(), "tool_calls") || !strings.Contains(w.Body.String(), "[DONE]") {
				t.Fatalf("stream: %d %s", w.Code, w.Body.String())
			}
			// All outgoing keys in this request belong to the chat-completions specification.
			for k := range got {
				switch k {
				case "model", "messages", "stream", "stream_options", "max_tokens", "tools":
				default:
					t.Errorf("non-spec/stripped field forwarded: %s", k)
				}
			}
			wantMax := float64(g.accounting().MaxOutputTokens)
			if got["max_tokens"] != wantMax || got["stream_options"].(map[string]any)["include_usage"] != true || !reflect.DeepEqual(got["tools"], body["tools"]) {
				t.Errorf("sanitized body: %v", got)
			}
			if headers.Get("Authorization") != "Bearer key" || headers.Get("X-OpenWebUI-User-Email") != "" || headers.Get("x-opencode-project") != "" {
				t.Errorf("headers: %v", headers)
			}
			if flavor == "openai" {
				for k := range headers {
					if strings.HasPrefix(strings.ToLower(k), "x-litellm-") {
						t.Errorf("header leaked: %s", k)
					}
				}
			} else if headers.Get("x-litellm-end-user-id") != "alice" || headers.Get("x-litellm-session-id") != "ses_abcdefgh" {
				t.Errorf("headers: %v", headers)
			}
			if len(rec.ev) != 1 || rec.ev[0].UID != "u1" || rec.ev[0].SessionID != "ses_abcdefgh" || rec.ev[0].ProjectID != "project" || *rec.ev[0].InTokens != 10 || *rec.ev[0].OutTokens != 2 {
				t.Fatalf("local usage: %+v", rec.ev)
			}
		})
	}
	for _, fields := range []string{"", `,"max_completion_tokens":42`, `,"max_tokens":3`, `,"max_tokens":1,"max_completion_tokens":1`} {
		b, _, err := normalize([]byte(`{"model":"m","messages":[]`+fields+`}`), true, 2)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		json.Unmarshal(b, &got)
		wantMax := float64(2)
		if strings.Contains(fields, `"max_tokens":1`) {
			wantMax = 1
		}
		if got["max_tokens"] != wantMax || got["stream_options"] != nil {
			t.Errorf("token bounds: %s", b)
		}
		if strings.Contains(fields, "max_completion_tokens") {
			if got["max_completion_tokens"] != wantMax {
				t.Errorf("completion token bound: %s", b)
			}
		} else if got["max_completion_tokens"] != nil {
			t.Errorf("injected completion limit: %s", b)
		}
	}
}

func TestProviderResponseHeaderAllowlist(t *testing.T) {
	for _, flavor := range []string{"litellm", "openai"} {
		for _, path := range []string{"/v1/models", "/v1/embeddings", "/v1/chat/completions"} {
			t.Run(flavor+path, func(t *testing.T) {
				allowed := map[string]string{"Content-Type": "application/json", "Content-Length": "2", "Cache-Control": "no-store",
					"X-Request-Id": "req-1", "Request-Id": "req-2", "Openai-Request-Id": "req-3", "Retry-After": "15"}
				g, _ := providerGateway(flavor, "key", func(w http.ResponseWriter, r *http.Request) {
					for k, v := range allowed {
						w.Header().Set(k, v)
					}
					for _, k := range []string{"X-Litellm-Api-Base", "X-Litellm-Response-Cost", "X-Litellm-Model-Id", "X-Debug", "Set-Cookie", "Server", "Location", "X-Request-Id-Debug"} {
						w.Header().Set(k, "private")
					}
					w.Header().Set("Trailer", "X-Debug-Trailer")
					if r.URL.Path == "/prefix/v1/chat/completions" {
						w.WriteHeader(http.StatusTooManyRequests)
					} else if r.URL.Path == "/prefix/v1/embeddings" {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					io.WriteString(w, "{}")
					w.Header().Set("X-Debug-Trailer", "private")
				})
				method, body := "POST", `{"model":"`+instance.Current().Models.Default+`","input":"x","messages":[]}`
				if path == "/v1/models" {
					method, body = "GET", ""
				}
				w := call(g, method, path, "10.90.1.10:1", body, nil)
				wantStatus := http.StatusOK
				if path == "/v1/chat/completions" {
					wantStatus = http.StatusTooManyRequests
				} else if path == "/v1/embeddings" {
					wantStatus = http.StatusServiceUnavailable
				}
				if w.Code != wantStatus {
					t.Fatal(w.Code)
				}
				for k, values := range w.Header() {
					if want, ok := allowed[k]; !ok || len(values) != 1 || values[0] != want {
						t.Errorf("unexpected header %s=%v", k, values)
					}
				}
				for k, v := range allowed {
					if w.Header().Get(k) != v {
						t.Errorf("lost %s", k)
					}
				}
			})
		}
	}
}

func TestOpenAIUsageCostCurrency(t *testing.T) {
	for _, currency := range []string{"EUR", "USD", "none"} {
		t.Run(currency, func(t *testing.T) {
			g, rec := providerGateway("openai", "key", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("x-litellm-response-cost", "99")
				w.Header().Set("x-litellm-model-id", "private")
				io.WriteString(w, `{"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.5}}`)
			})
			g.Accounting.UpstreamCostCurrency, g.Accounting.USDPerEUR = currency, 1.25
			w := call(g, "POST", "/v1/chat/completions", "10.90.1.10:1", `{"model":"`+instance.Current().Models.Default+`","messages":[]}`, nil)
			if w.Code != 200 || len(rec.ev) != 1 {
				t.Fatalf("response: %d events: %+v", w.Code, rec.ev)
			}
			e := rec.ev[0]
			if e.DeploymentID != "" || e.RawCostSource != "" || e.RawCostUnit != "" || e.RawCost != nil {
				t.Fatalf("raw evidence: %+v", e)
			}
			if e.CostSource != "estimate" || e.CostEUR == nil || *e.CostEUR >= .4 {
				t.Errorf("billing: %+v", e)
			}
		})
	}
}

func TestSuggestUsesInstancePrompt(t *testing.T) {
	for _, name := range []string{"minimal", "example"} {
		t.Run(name, func(t *testing.T) {
			d, err := instance.Load("../../../../instance/" + name + "/instance.yaml")
			if err != nil {
				t.Fatal(err)
			}
			d.Copy["folder_name_prompt"] = instance.Localized{EN: "Instance prompt for " + name, DE: "Alternative prompt"}
			t.Cleanup(instance.Select(d))
			called := false
			g, _ := providerGateway("openai", "", func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[0].Content != "Instance prompt for "+name {
					t.Fatalf("wrong prompt: %+v", body.Messages)
				}
				called = true
				io.WriteString(w, `{"choices":[{"message":{"content":"folder-name"}}]}`)
			})
			if got, err := g.Suggest(context.Background(), store.User{UID: "u1"}, "a task"); err != nil || got != "folder-name" || !called {
				t.Fatalf("suggest = %q, %v, called %v", got, err, called)
			}
		})
	}
}
