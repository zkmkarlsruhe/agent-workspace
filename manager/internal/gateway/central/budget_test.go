// SPDX-License-Identifier: MIT

package central

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/llm"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// At the daily cap (or the concurrency limit) a tools/call never reaches
// LiteLLM; an admitted one is charged MCPChargeEUR before its hold ends.
func TestCallNeedsBudget(t *testing.T) {
	s, f, r := setup(t, true)
	refuse := llm.ErrDayCap
	released := 0
	s.Reserve = func(_ context.Context, uid string, eur float64) (func(), error) {
		if refuse != nil {
			return nil, refuse
		}
		if uid != "u1" || eur != llm.MCPChargeEUR {
			t.Errorf("reserve %s %v", uid, eur)
		}
		return func() {
			if len(r.ev) == 0 {
				t.Error("released before the charge was recorded")
			}
			released++
		}, nil
	}
	const call = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_workspace_wiki","arguments":{}}}`
	called := func() bool { return slices.Contains(f.calls, "tools/call:workspace_kb-search_workspace_wiki") }
	if w := rpc(s, me, "workspace-knowledge", call); !strings.Contains(w.Body.String(), `"isError":true`) ||
		!strings.Contains(w.Body.String(), "Tagesbudget") || called() || len(r.ev) != 0 {
		t.Fatalf("over the cap: %s, upstream %v", w.Body, f.calls)
	}
	refuse = llm.ErrBusy
	if w := rpc(s, me, "workspace-knowledge", call); !strings.Contains(w.Body.String(), "gleichzeitige") || called() {
		t.Fatalf("busy: %s", w.Body)
	}
	refuse = nil
	if w := rpc(s, me, "workspace-knowledge", call); strings.Contains(w.Body.String(), `"isError":true`) || !called() {
		t.Fatalf("admitted: %s", w.Body)
	}
	if len(r.ev) != 1 || r.ev[0].CostEUR == nil || *r.ev[0].CostEUR != llm.MCPChargeEUR || r.ev[0].CostSource != "estimate" || released != 1 {
		t.Fatalf("charge: %+v, released %d", r.ev, released)
	}
}

func TestConfiguredMCPCharge(t *testing.T) {
	for _, price := range []float64{0, .07} {
		r := &rec{}
		s := &Server{ChargeEUR: &price, Usage: r, Log: slog.New(slog.DiscardHandler)}
		s.meter(context.Background(), store.User{UID: "u1"}, "", "test", time.Now(), 200)
		if len(r.ev) != 1 || r.ev[0].CostEUR == nil || *r.ev[0].CostEUR != price || r.ev[0].CostSource != "estimate" {
			t.Fatalf("%+v", r.ev)
		}
	}
}
