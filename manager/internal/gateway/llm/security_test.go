// SPDX-License-Identifier: MIT

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

var blockedRequestKeys = []string{"user", "metadata", "litellm_metadata", "litellm_session_id", "litellm_trace_id", "tags",
	"fallbacks", "context_window_fallbacks", "num_retries", "timeout", "mock_response", "mock_tool_calls",
	"mock_testing_fallbacks", "caching", "cache", "guardrails", "spend_logs_metadata", "api_base", "base_url",
	"api_key", "api_version", "input_cost_per_token", "output_cost_per_token", "model_info", "custom_llm_provider", "extra_headers",
	"input_cost_per_second", "output_cost_per_second", "future_unknown_key"}

func TestRequestAllowlist(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/embeddings"} {
		for _, flavor := range []string{"litellm", "openai"} {
			t.Run(flavor+path, func(t *testing.T) {
				body := map[string]any{"model": instance.Current().Models.Default, "messages": []any{}, "input": "hello", "encoding_format": "float", "dimensions": 128,
					"tool_choice": "auto", "temperature": 0.2, "top_p": 1, "max_completion_tokens": 10, "stop": "stop", "response_format": map[string]any{"type": "json_object"},
					"seed": 1, "reasoning_effort": "low", "parallel_tool_calls": true, "logprobs": true, "top_logprobs": 2, "frequency_penalty": 0, "presence_penalty": 0, "logit_bias": map[string]any{}, "n": 1}
				for _, key := range blockedRequestKeys {
					body[key] = "untrusted"
				}
				called := false
				g, _ := providerGateway(flavor, "key", func(w http.ResponseWriter, r *http.Request) {
					called = true
					var got map[string]any
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Fatal(err)
					}
					for _, key := range blockedRequestKeys {
						if _, ok := got[key]; ok {
							t.Errorf("forwarded %s", key)
						}
						delete(body, key)
					}
					for key := range body {
						if key == "n" && path == "/v1/chat/completions" {
							continue
						}
						if _, ok := got[key]; !ok {
							t.Errorf("lost allowed key %s", key)
						}
					}
					io.WriteString(w, `{}`)
				})
				raw, _ := json.Marshal(body)
				w := call(g, "POST", path, "10.90.1.10:1", string(raw), nil)
				if w.Code != 200 || !called {
					t.Fatalf("status=%d called=%v", w.Code, called)
				}
			})
		}
	}
}

func TestNormalizeNullReturnsStructured400(t *testing.T) {
	for _, body := range []string{`null`, `{"stream":true,"stream_options":null}`, `{"stream_options":null}`, `{"stream":true,"stream_options":[]}`, `{"stream":true,"stream_options":false}`} {
		g, rec := providerGateway("litellm", "key", func(http.ResponseWriter, *http.Request) { t.Fatal("invalid request reached upstream") })
		w := call(g, "POST", "/v1/chat/completions", "10.90.1.10:1", body, nil)
		var result struct{ Error struct{ Type string } }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 400 || result.Error.Type != "invalid_request_error" || len(rec.ev) != 0 {
			t.Fatalf("body=%s response=%d %s events=%v", body, w.Code, w.Body, rec.ev)
		}
	}
}

func TestUsageBounds(t *testing.T) {
	for _, field := range []string{"prompt_tokens", "completion_tokens", "cached_tokens", "reasoning_tokens"} {
		for _, flavor := range []string{"litellm", "openai"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", field, flavor, stream), func(t *testing.T) {
					for _, over := range []bool{false, true} {
						limit := int64(100)
						if field == "prompt_tokens" || field == "cached_tokens" {
							limit = 1000
						}
						if over {
							limit++
						}
						usage := map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "cost": .001}
						switch field {
						case "cached_tokens":
							usage["prompt_tokens_details"] = map[string]any{field: limit}
						case "reasoning_tokens":
							usage["completion_tokens_details"] = map[string]any{field: limit}
						default:
							usage[field] = limit
						}
						raw, _ := json.Marshal(map[string]any{"usage": usage})
						g, rec := providerGateway(flavor, "key", func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("x-litellm-response-cost", "0.001")
							if stream {
								w.Header().Set("Content-Type", "text/event-stream")
								fmt.Fprintf(w, "data: %s\n\n", raw)
								if over {
									io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"cost\":0.001}}\n\n")
								}
							} else {
								w.Write(raw)
							}
						})
						g.Accounting.LargestContext, g.Accounting.MaxOutputTokens = 1000, 100
						body := fmt.Sprintf(`{"model":%q,"messages":[],"stream":%v}`, instance.Current().Models.Default, stream)
						w := call(g, "POST", "/v1/chat/completions", "10.90.1.10:1", body, nil)
						if w.Code != 200 || len(rec.ev) != 1 {
							t.Fatalf("status=%d events=%v", w.Code, rec.ev)
						}
						e := rec.ev[0]
						if over {
							if e.CostEUR == nil || *e.CostEUR != g.Accounting.ReserveEUR() || e.CostSource != "estimate" || e.InTokens != nil || e.OutTokens != nil || e.CachedTokens != nil || e.Reasoning != nil || e.WhHi != nil {
								t.Fatalf("unbounded usage: %+v", e)
							}
						} else if e.InTokens == nil || e.OutTokens == nil {
							t.Fatalf("boundary rejected: %+v", e)
						}
					}
				})
			}
		}
	}
}

func TestDerivedCostBounds(t *testing.T) {
	for _, price := range []float64{math.MaxFloat64, math.Inf(1), math.NaN(), 1e12} {
		a := instance.DefaultAccounting()
		a.Prices = map[string]instance.TokenPrice{"standard": {Input: price, Output: price}}
		a.EmbeddingEURPerMillion = price
		for _, capability := range []string{"llm", "embedding"} {
			e := store.UsageEvent{Status: 200, Capability: capability, InTokens: new(int64(100)), OutTokens: new(int64(100))}
			charge(&e, a)
			if e.CostEUR != nil || e.CostSource != "unknown" {
				t.Fatalf("invalid estimate recorded: %+v", e)
			}
		}
	}
	a := instance.Current().Accounting
	e := store.UsageEvent{Status: 200, Capability: "llm", InTokens: new(int64(math.MaxInt64)), OutTokens: new(int64(math.MaxInt64))}
	charge(&e, a)
	if e.CostEUR == nil || *e.CostEUR != a.ReserveEUR() || e.CostSource != "estimate" {
		t.Fatalf("overflow: %+v", e)
	}
}

func TestOpenAIConfiguredModels(t *testing.T) {
	d := instance.Current()
	models := []string{d.Models.Default, d.Models.Small, d.Models.Thorough}
	// Variants add model settings; their upstream model IDs remain the role aliases.
	for _, p := range d.Assistants {
		models = append(models, map[string]string{"default": d.Models.Default, "small": d.Models.Small, "thorough": d.Models.Thorough}[p.ModelRole])
	}
	models = append(models, "unconfigured-expensive-model", "", d.Models.Default+"#unknown-variant")
	for _, model := range models {
		for _, flavor := range []string{"openai", "litellm"} {
			called := false
			g, _ := providerGateway(flavor, "key", func(w http.ResponseWriter, r *http.Request) { called = true; io.WriteString(w, `{}`) })
			raw, _ := json.Marshal(map[string]any{"model": model, "messages": []any{}})
			w := call(g, "POST", "/v1/chat/completions", "10.90.1.10:1", string(raw), nil)
			allowed := flavor != "openai" || model == d.Models.Default || model == d.Models.Small || model == d.Models.Thorough
			if allowed {
				if w.Code != 200 || !called {
					t.Fatalf("configured model %q: %d", model, w.Code)
				}
			} else if w.Code != 400 || called || !strings.Contains(w.Body.String(), `"type":"invalid_request_error"`) {
				t.Fatalf("unconfigured model %q: %d called=%v", model, w.Code, called)
			}
		}
	}
}

func TestHandwritingProviderFields(t *testing.T) {
	for _, flavor := range []string{"litellm", "openai"} {
		t.Run(flavor, func(t *testing.T) {
			called := false
			g, rec := providerGateway(flavor, "key", func(w http.ResponseWriter, r *http.Request) {
				called = true
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["temperature"] != float64(0) {
					t.Fatalf("missing transcription setting: %v", body)
				}
				if flavor == "openai" {
					for key := range r.Header {
						if strings.HasPrefix(strings.ToLower(key), "x-litellm-") {
							t.Errorf("leaked header %s", key)
						}
					}
					if _, ok := body["disable_fallbacks"]; ok {
						t.Error("forwarded disable_fallbacks")
					}
					if _, ok := body["num_retries"]; ok {
						t.Error("forwarded num_retries")
					}
				} else if body["disable_fallbacks"] != true || body["num_retries"] != float64(0) || r.Header.Get("x-litellm-end-user-id") != "person" || r.Header.Get("x-litellm-spend-logs-metadata") == "" {
					t.Fatalf("lost LiteLLM settings: %v %v", body, r.Header)
				}
				io.WriteString(w, `{"choices":[{"message":{"content":"text"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.5}}`)
			})
			ctx := logx.Set(context.Background(), logx.Fields{ReqID: "req-test"})
			result, err := g.Transcribe(ctx, store.User{UID: "u1", Username: "person"}, "ses_abcdefgh", "handwriting-model", []byte("png"))
			if len(rec.ev) != 1 {
				t.Fatalf("events: %v", rec.ev)
			}
			if flavor == "openai" && (rec.ev[0].RawCost != nil || rec.ev[0].CostSource != "estimate") {
				t.Fatalf("internal usage.cost trusted: %+v", rec.ev[0])
			}
			if err != nil || result.Text != "text" || !called {
				t.Fatalf("%+v %v called=%v", result, err, called)
			}
		})
	}
}

func TestDroppedRequestFieldDebugLog(t *testing.T) {
	var logs bytes.Buffer
	before := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(before)
	_, _, err := normalize([]byte(`{"model":"m","extra_headers":{"secret":"private-value"}}`), true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"level":"DEBUG"`) || !strings.Contains(logs.String(), `"key":"extra_headers"`) || strings.Contains(logs.String(), "private-value") {
		t.Fatalf("debug log: %s", logs.String())
	}
}
