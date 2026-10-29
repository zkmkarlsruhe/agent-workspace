// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestUpstreamCostUnits(t *testing.T) {
	for _, tc := range []struct {
		unit            string
		rate, raw, want float64
		source, label   string
	}{
		{"EUR", 0, 2, 2, "header", "header"},
		{"EUR", 0, 3, 3, "usage", "usage"},
		{"USD", 2, 4, 2, "header", "header-usd"},
		{"USD", 2, 4, 2, "usage", "usage-usd"},
		{"none", 0, 2, 0, "usage", "unknown"},
		{"USD", 0, 2, 0, "header", "unknown"},
		{"USD", 0.001, 2, 0, "usage", "unknown"},
		{"EUR", 0, -1, 0, "usage", "unknown"},
		{"EUR", 0, 1001, 0, "header", "unknown"},
	} {
		t.Run(tc.unit+"/"+tc.source+"/"+tc.label, func(t *testing.T) {
			a := instance.Accounting{UpstreamCostCurrency: tc.unit, USDPerEUR: tc.rate}
			e := store.UsageEvent{Status: 200, Capability: "embedding"}
			upstreamCost(&e, tc.raw, tc.source, a)
			charge(&e, a)
			if e.CostSource != tc.label || (tc.want == 0 && e.CostEUR != nil) || (tc.want > 0 && (e.CostEUR == nil || *e.CostEUR != tc.want)) {
				t.Fatalf("cost=%v source=%s", e.CostEUR, e.CostSource)
			}
			if e.RawCost == nil || *e.RawCost != tc.raw || e.RawCostUnit != tc.unit || e.RawCostSource != tc.source {
				t.Fatalf("raw: %+v", e)
			}
		})
	}
	a := instance.Accounting{UpstreamCostCurrency: "EUR"}
	e := store.UsageEvent{}
	upstreamCost(&e, 2, "header", a)
	upstreamCost(&e, -1, "usage", a)
	if *e.CostEUR != 2 || *e.RawCost != 2 || e.CostSource != "header" {
		t.Fatal("invalid usage replaced valid header")
	}
	upstreamCost(&e, 3, "usage", a)
	if *e.CostEUR != 3 || *e.RawCost != 3 || e.CostSource != "usage" {
		t.Fatal("valid usage did not win")
	}
}

func TestChargeRolePrices(t *testing.T) {
	a := instance.DefaultAccounting()
	a.LargestContext, a.MaxOutputTokens = 1000, 100
	a.Prices = map[string]instance.TokenPrice{"standard": {Input: 1, Output: 2}, "fast": {Input: .1, Output: .2}}
	for _, tc := range []struct {
		name, alias string
		in, out     *int64
		want        float64
	}{
		{"standard", instance.Current().Models.Default, new(int64(100)), new(int64(10)), .00012},
		{"fast", instance.Current().Models.Small, new(int64(100)), new(int64(10)), .000012},
		{"missing role", instance.Current().Models.Thorough, new(int64(100)), new(int64(10)), .00012},
		{"unknown alias", "unlisted", new(int64(100)), new(int64(10)), .00012},
		{"missing output", "unlisted", new(int64(100)), nil, .0003},
		{"missing input", "unlisted", nil, new(int64(10)), .00102},
		{"missing usage", "unlisted", nil, nil, .1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := store.UsageEvent{Capability: "llm", Status: 200, Target: tc.alias, InTokens: tc.in, OutTokens: tc.out}
			charge(&e, a)
			if e.CostSource != "estimate" || e.CostEUR == nil || math.Abs(*e.CostEUR-tc.want) > 1e-12 {
				t.Fatalf("%+v", e)
			}
		})
	}
	a.Prices = map[string]instance.TokenPrice{}
	e := store.UsageEvent{Capability: "llm", Status: 200}
	charge(&e, a)
	if e.CostSource != "unknown" || e.CostEUR != nil || a.ReserveEUR() != 0 {
		t.Fatalf("disabled: %+v", e)
	}
}

func TestEmbeddingUnknownCost(t *testing.T) {
	for _, price := range []float64{0, 2} {
		e := store.UsageEvent{Capability: "embedding", Status: 200, InTokens: new(int64(100))}
		charge(&e, instance.Accounting{EmbeddingEURPerMillion: price, LargestContext: 262144, MaxOutputTokens: 32768})
		if price == 0 {
			if e.CostEUR != nil || e.CostSource != "unknown" {
				t.Fatalf("%+v", e)
			}
		} else if e.CostEUR == nil || *e.CostEUR != .0002 || e.CostSource != "estimate" {
			t.Fatalf("%+v", e)
		}
	}
}

type accountingTransport func(*http.Request) (*http.Response, error)

func (f accountingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise public, internal and image paths without sockets or a paid provider.
func TestAccountingPipeline(t *testing.T) {
	for _, kind := range []string{"chat", "stream", "embedding", "internal", "image"} {
		t.Run(kind, func(t *testing.T) {
			a := instance.Current().Accounting
			a.UpstreamCostCurrency, a.USDPerEUR = "USD", 2
			recorder := &rec{}
			u, _ := url.Parse("http://provider.test")
			g := New(Gateway{Accounting: &a, Upstream: u, SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{}, Usage: recorder, Log: slog.New(slog.DiscardHandler)})
			g.proxy.Transport = accountingTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.04},"choices":[{"message":{"content":"answer"}}],"data":[{"b64_json":"eA=="}]}`
				h := http.Header{"Content-Type": {"application/json"}, "X-Litellm-Response-Cost": {"0.02"}}
				if kind == "stream" {
					h.Set("Content-Type", "text/event-stream")
					body = "data: " + body + "\n\n"
				}
				return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			switch kind {
			case "image":
				if _, _, err := g.Image(context.Background(), store.User{UID: "u1"}, "", []string{"test"}, "test", "1024x1024", nil); err != nil {
					t.Fatal(err)
				}
			case "internal":
				if _, err := g.Complete(context.Background(), store.User{UID: "u1"}, "test", "test", 20); err != nil {
					t.Fatal(err)
				}
			default:
				path := "/v1/chat/completions"
				if kind == "embedding" {
					path = "/v1/embeddings"
				}
				r := httptest.NewRequest("POST", path, strings.NewReader(`{"model":"test","messages":[]}`))
				r.RemoteAddr = "10.90.1.10:1234"
				w := httptest.NewRecorder()
				g.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatal(w.Code)
				}
			}
			if len(recorder.ev) != 1 {
				t.Fatalf("events: %d", len(recorder.ev))
			}
			e := recorder.ev[0]
			if e.CostEUR == nil || *e.CostEUR != .02 || e.CostSource != "usage-usd" || e.RawCost == nil || *e.RawCost != .04 || e.RawCostUnit != "USD" || e.DeploymentID != "" {
				t.Fatalf("%+v", e)
			}
		})
	}
}

func TestConfiguredImageCharge(t *testing.T) {
	for _, price := range []float64{0, .7} {
		a := instance.DefaultAccounting()
		a.ImageEUR = price
		recorder := &rec{}
		u, _ := url.Parse("http://provider.test")
		budget := &Budget{CapEUR: 5, Now: time.Now, Spent: func(context.Context, string, time.Time) (float64, error) { return 0, nil }}
		g := New(Gateway{Accounting: &a, Upstream: u, Budget: budget, Usage: recorder, Log: slog.New(slog.DiscardHandler)})
		g.proxy.Transport = accountingTransport(func(r *http.Request) (*http.Response, error) {
			if budget.held["u1"].eur != price {
				t.Fatal("image hold did not use configured price")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"data":[{"b64_json":"eA=="}]}`)), Request: r}, nil
		})
		if _, _, err := g.Image(context.Background(), store.User{UID: "u1"}, "", []string{"test"}, "test", "1024x1024", nil); err != nil {
			t.Fatal(err)
		}
		if len(recorder.ev) != 1 || recorder.ev[0].CostEUR == nil || *recorder.ev[0].CostEUR != price || recorder.ev[0].CostSource != "estimate" || len(budget.held) != 0 {
			t.Fatalf("%+v", recorder.ev)
		}
	}
}

func TestInstanceLegacyAccounting(t *testing.T) {
	d, err := instance.Load("../../../../instance/example/instance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	a := d.Accounting
	for _, tc := range []struct {
		name      string
		got, want float64
	}{
		{"reserve", a.ReserveEUR(), 0.30},
		{"mcp", a.MCPEUR, MCPChargeEUR},
		{"toolbox", a.ToolboxEUR, ToolboxChargeEUR},
		{"search", a.SearchEUR, SearchChargeEUR},
		{"fetch", a.FetchEUR, FetchChargeEUR},
		{"image", a.ImageEUR, ImageReserveEUR},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: got %g EUR, want %g EUR", tc.name, tc.got, tc.want)
		}
	}
}
