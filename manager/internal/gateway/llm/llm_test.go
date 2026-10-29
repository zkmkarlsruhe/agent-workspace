// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type users struct{}

func (users) UserBySlot(_ context.Context, slot int) (store.User, error) {
	if slot != 1 {
		return store.User{}, store.ErrNotFound
	}
	return store.User{UID: "u1", Username: "alice", Slot: 1}, nil
}

type rec struct {
	mu sync.Mutex
	ev []store.UsageEvent
}

func (r *rec) InsertUsage(_ context.Context, e store.UsageEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev = append(r.ev, e)
	return nil
}

func gateway(t *testing.T, upstream http.HandlerFunc) (*Gateway, *rec) {
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	cfg := config.Config{UserNetPrefix: "10.90"}
	r := &rec{}
	a := instance.Current().Accounting
	a.UpstreamCostCurrency = "EUR"
	return New(Gateway{Accounting: &a, Upstream: u, APIKey: "sk-unified", SlotOf: cfg.SlotOf, Users: users{}, Usage: r,
		Log: slog.New(slog.DiscardHandler)}), r
}

func call(g *Gateway, method, path, remote, body string, h map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = remote
	for k, v := range h {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

func TestStreamingPipeline(t *testing.T) {
	for _, tc := range []struct {
		name, cost, source string
		wantEUR            float64
	}{
		{"usage cost", `"cost":0.0012,`, "usage", 0.0012},
		{"estimated cost", "", "estimate", 0.0000852},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotH http.Header
			var gotBody map[string]any
			g, rec := gateway(t, func(w http.ResponseWriter, r *http.Request) {
				gotH = r.Header.Clone()
				json.NewDecoder(r.Body).Decode(&gotBody)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("x-litellm-model-id", "dep-42")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hal\"}}]}\n\n")
				w.(http.Flusher).Flush()
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,"+tc.cost+"\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":40},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n")
				io.WriteString(w, "data: [DONE]\n\n")
			})
			body := `{"model":"default","stream":true,"user":"evil","metadata":{"x":1},"api_base":"http://x","messages":[]}`
			w := call(g, "POST", "/v1/chat/completions", "10.90.1.10:5000", body, map[string]string{
				"Content-Type":           "application/json",
				"Authorization":          "Bearer user-own-key",
				"x-litellm-end-user-id":  "someone-else",
				"x-litellm-tags":         "free",
				"X-OpenWebUI-User-Email": "boss@example.org",
				"x-opencode-session-id":  "ses_abcdefgh1234",
				"x-opencode-session":     "ses_rootroot0001",
				"x-opencode-project":     "proj-1",
				"traceparent":            "00-abc",
				"x-session-id":           "spoof",
			})
			if w.Code != 200 || !strings.Contains(w.Body.String(), "[DONE]") {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if gotH.Get("Authorization") != "Bearer sk-unified" || gotH.Get("x-litellm-end-user-id") != "alice" ||
				gotH.Get("x-litellm-session-id") != "ses_abcdefgh1234" || gotH.Get("x-litellm-call-id") == "" {
				t.Fatalf("upstream headers %v", gotH)
			}
			for _, k := range []string{"x-litellm-tags", "X-OpenWebUI-User-Email", "traceparent", "x-session-id", "x-opencode-session-id"} {
				if gotH.Get(k) != "" {
					t.Errorf("%s leaked", k)
				}
			}
			for _, k := range []string{"user", "metadata", "api_base"} {
				if _, ok := gotBody[k]; ok {
					t.Errorf("body key %s leaked", k)
				}
			}
			if so, _ := gotBody["stream_options"].(map[string]any); so["include_usage"] != true {
				t.Fatal("include_usage not forced")
			}
			if len(rec.ev) != 1 {
				t.Fatalf("want one event, got %d", len(rec.ev))
			}
			e := rec.ev[0]
			if e.UID != "u1" || e.Target != "default" || e.DeploymentID != "dep-42" || e.SessionID != "ses_abcdefgh1234" ||
				e.ProjectID != "proj-1" || *e.InTokens != 100 || *e.CachedTokens != 40 || *e.OutTokens != 7 || *e.Reasoning != 3 ||
				e.FinishReason != "stop" || e.EventID != gotH.Get("x-litellm-call-id") || e.Status != 200 ||
				e.RootSessionID != "ses_rootroot0001" {
				t.Fatalf("event %+v", e)
			}
			// 7 out × 5.8 J + 100 in × 1.2 × 0.061 J (high), 7 × 1.45 J + 60 × 0.0732 J (low)
			if e.MethodVersion != "2026-10-a" || e.CO2LoG == nil || e.CO2HiG == nil || e.WhLo == nil ||
				math.Abs(*e.WhHi-(7*5.8+100*1.2*0.061)/3600) > 1e-12 || math.Abs(*e.WhLo-(7*5.8/4+60*1.2*0.061)/3600) > 1e-12 ||
				math.Abs(*e.CO2HiG-*e.WhHi*0.344) > 1e-12 {
				t.Fatalf("co2 %+v", e)
			}
			if e.CostEUR == nil || math.Abs(*e.CostEUR-tc.wantEUR) > 1e-12 || e.CostSource != tc.source {
				t.Fatalf("cost=%v source=%q, want %g %q", e.CostEUR, e.CostSource, tc.wantEUR, tc.source)
			}
		})
	}
}

func TestJSONCostAndRejections(t *testing.T) {
	g, rec := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-litellm-response-cost", "0.0012")
		io.WriteString(w, `{"data":[],"usage":{"prompt_tokens":5}}`)
	})
	if w := call(g, "POST", "/v1/embeddings", "10.90.1.10:1", `{"model":"e","input":"x"}`, nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if e := rec.ev[0]; e.Capability != "embedding" || *e.CostEUR != 0.0012 || e.CostSource != "header" || *e.InTokens != 5 ||
		e.CO2HiG != nil || e.MethodVersion != "" {
		t.Fatalf("%+v", e)
	}
	if w := call(g, "GET", "/v1/models", "10.90.1.10:1", "", nil); w.Code != 200 || len(rec.ev) != 1 {
		t.Fatal("models must pass and not be metered")
	}
	cases := []struct {
		method, path, remote string
		code                 int
	}{
		{"POST", "/v1/chat/completions", "10.90.1.2:1", 403},  // egress address, not the person
		{"POST", "/v1/chat/completions", "172.20.0.5:1", 403}, // edge network
		{"POST", "/v1/chat/completions", "10.90.7.10:1", 403}, // unknown slot
		{"POST", "/v1/responses", "10.90.1.10:1", 404},
		{"GET", "/key/generate", "10.90.1.10:1", 404},
	}
	for _, c := range cases {
		w := call(g, c.method, c.path, c.remote, "{}", nil)
		if w.Code != c.code || w.Header().Get("x-should-retry") != "false" {
			t.Errorf("%s %s from %s: %d", c.method, c.path, c.remote, w.Code)
		}
	}
}

// A stream that ends before its usage chunk (client or upstream abort) has
// no cost from LiteLLM: it is charged the conservative estimate, never zero.
// Output tokens and n are bounded before the call leaves.
func TestAbortedStreamCharged(t *testing.T) {
	var got map[string]any
	g, rec := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"1 2 3\"}}]}\n\n")
		w.(http.Flusher).Flush()
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	})
	call(g, "POST", "/v1/chat/completions", "10.90.1.10:1",
		`{"model":"default","stream":true,"max_tokens":999999,"max_completion_tokens":999999,"n":8,"messages":[]}`, nil)
	if got["max_tokens"] != float64(g.accounting().MaxOutputTokens) || got["max_completion_tokens"] != float64(g.accounting().MaxOutputTokens) || got["n"] != nil {
		t.Fatalf("not bounded: %v", got)
	}
	if len(rec.ev) != 1 || rec.ev[0].CostEUR == nil || *rec.ev[0].CostEUR != g.accounting().ReserveEUR() || rec.ev[0].CostSource != "estimate" {
		t.Fatalf("aborted stream: %+v", rec.ev)
	}
	// no max_tokens at all: the bound is set
	call(g, "POST", "/v1/chat/completions", "10.90.1.10:1", `{"model":"default","stream":true,"messages":[]}`, nil)
	if got["max_tokens"] != float64(g.accounting().MaxOutputTokens) {
		t.Fatalf("missing max_tokens: %v", got)
	}
}

// A call cancelled after it reached LiteLLM but before the response headers
// may already be generating (billable): it keeps the conservative charge. A
// call that never left (connection refused) costs nothing.
func TestCancelledBeforeHeadersCharged(t *testing.T) {
	arrived := make(chan struct{})
	g, rec := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		close(arrived)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-arrived; cancel() }()
	r := httptest.NewRequestWithContext(ctx, "POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"default","max_tokens":32768,"messages":[]}`))
	r.RemoteAddr = "10.90.1.10:1"
	g.ServeHTTP(httptest.NewRecorder(), r)
	if len(rec.ev) != 1 || rec.ev[0].CostEUR == nil || *rec.ev[0].CostEUR != g.accounting().ReserveEUR() || rec.ev[0].CostSource != "estimate" {
		t.Fatalf("cancelled after dispatch: %+v", rec.ev)
	}

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	l.Close() // nothing listens there
	g.Upstream, _ = url.Parse("http://" + l.Addr().String())
	call(g, "POST", "/v1/chat/completions", "10.90.1.10:1", `{"model":"default","messages":[]}`, nil)
	if len(rec.ev) != 2 || rec.ev[1].CostEUR != nil || rec.ev[1].Status != http.StatusBadGateway {
		t.Fatalf("never sent: %+v", rec.ev[1:])
	}
	if _, err := g.Complete(context.Background(), store.User{UID: "u1", Username: "alice"}, "s", "u", 5); err == nil ||
		len(rec.ev) != 3 || rec.ev[2].CostEUR != nil {
		t.Fatalf("Complete never sent: %v %+v", err, rec.ev[2:])
	}
}

func TestSuggestMetered(t *testing.T) {
	g, rec := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-litellm-end-user-id") != "alice" || r.Header.Get("Authorization") != "Bearer sk-unified" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":" besucher-auswertung\n"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":4}}`)
	})
	name, err := g.Suggest(context.Background(), store.User{UID: "u1", Username: "alice"}, "Besucherzahlen auswerten")
	if err != nil || name != "besucher-auswertung" {
		t.Fatal(name, err)
	}
	if len(rec.ev) != 1 || *rec.ev[0].InTokens != 30 || rec.ev[0].Target != "small" {
		t.Fatalf("%+v", rec.ev)
	}
}

func TestSlotOfOnlyUserAddress(t *testing.T) {
	c := config.Config{UserNetPrefix: "10.90"}
	if s, ok := c.SlotOf(netip.MustParseAddr("10.90.12.10")); !ok || s != 12 {
		t.Fatal(s, ok)
	}
	for _, a := range []string{"10.90.12.3", "10.90.0.10", "10.91.1.10", "::1"} {
		if _, ok := c.SlotOf(netip.MustParseAddr(a)); ok {
			t.Error(a)
		}
	}
}

func TestUntrustedUsage(t *testing.T) {
	for _, tc := range []struct{ cost, currency string }{
		{"-99999", "EUR"}, {"+Inf", "EUR"}, {"NaN", "EUR"}, {"1e999", "EUR"},
		{"1001", "EUR"}, {"0", "EUR"}, {"0.02", "EUR"}, {"0.02", "none"},
	} {
		cost := tc.cost
		for _, mode := range []string{"stream", "header", "complete", "image"} {
			t.Run(tc.currency+"/"+mode+"/"+cost, func(t *testing.T) {
				up, _ := url.Parse("http://model.test")
				record := &rec{}
				a := instance.DefaultAccounting()
				a.UpstreamCostCurrency = tc.currency
				a.Prices = map[string]instance.TokenPrice{
					"standard": {Input: 0.60, Output: 3.60},
					"fast":     {Input: 0.60, Output: 3.60},
					"thorough": {Input: 0.60, Output: 3.60},
				}
				g := New(Gateway{Accounting: &a, Budget: &Budget{Accounting: &a}, Upstream: up, Users: users{}, Usage: record, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Log: slog.New(slog.DiscardHandler)})
				g.proxy.Transport = assistantTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if mode == "stream" {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":-100,\"completion_tokens\":-7,\"cost\":"+cost+"}}\n\n")
						return
					}
					w.Header().Set("x-litellm-response-cost", cost)
					io.WriteString(w, `{"choices":[{"message":{"content":"answer"}}],"data":[{"b64_json":"UE5H"}]}`)
				})}
				u := store.User{UID: "u1", Username: "alice"}
				switch mode {
				case "complete":
					if _, err := g.Complete(context.Background(), u, "", "test", 10); err != nil {
						t.Fatal(err)
					}
				case "image":
					if _, _, err := g.Image(context.Background(), u, "", []string{"image"}, "test", "1024x1024", nil); err != nil {
						t.Fatal(err)
					}
				default:
					if w := call(g, "POST", "/v1/chat/completions", "10.90.1.10:1", `{"model":"test","messages":[]}`, nil); w.Code != 200 {
						t.Fatal(w.Code)
					}
				}
				if len(record.ev) != 1 {
					t.Fatalf("events: %+v", record.ev)
				}
				e := record.ev[0]
				if e.CostEUR == nil || *e.CostEUR < 0 || *e.CostEUR > costCeiling || math.IsNaN(*e.CostEUR) || math.IsInf(*e.CostEUR, 0) {
					t.Fatalf("poisoned cost: %+v", e)
				}
				if (cost != "0.02" || tc.currency == "none") && e.CostSource != "estimate" {
					t.Fatalf("invalid cost accepted: %+v", e)
				}
				if mode == "stream" && cost == "-99999" && (e.InTokens == nil || *e.InTokens != 0 || e.OutTokens == nil || *e.OutTokens != 0) {
					t.Fatalf("negative tokens: %+v", e)
				}
				if cost == "0.02" && tc.currency == "EUR" {
					source := "header"
					if mode == "stream" {
						source = "usage"
					}
					if *e.CostEUR != 0.02 || e.CostSource != source {
						t.Fatalf("valid cost lost: %+v", e)
					}
				}
				if tc.currency == "none" {
					want := a.ReserveEUR()
					if mode == "stream" {
						want = 0 // Reported negative token counts clamp to zero.
					}
					if mode == "image" {
						want = a.ImageEUR
					}
					if *e.CostEUR != want {
						t.Fatalf("estimate: got %g, want %g", *e.CostEUR, want)
					}
				}
			})
		}
	}
	for _, cost := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		if validCostEUR(cost) {
			t.Fatalf("accepted %v", cost)
		}
	}
}
