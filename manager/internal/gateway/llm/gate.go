// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
)

// Gate guards the whole llm-gw listener (LLM routes and Workspace-MCP): no request
// of a person without consent to the current notice, and no LLM call once
// the person's spend of the day reached the cap. Requests without a known
// person pass to Next, which refuses them itself.
type Gate struct {
	// RuntimeAccess rejects staged pins that still belong to the shared runtime.
	RuntimeAccess func(context.Context, string) error
	SlotOf        func(netip.Addr) (int, bool)
	Users         Users
	Consented     func(ctx context.Context, uid string) (bool, error)
	Budget        *Budget // nil = no cap
	Portal        string  // https://apps.example.org, named in the refusal
	Next          http.Handler
}

var (
	ErrDayCap = errors.New("llm: daily budget reached")
	ErrBusy   = errors.New("llm: too many calls in flight")
)

// Every call holds the configured estimate until its usage is recorded.
// Both flavors bound chat output tokens; the hold is not a billing ceiling.
const maxInFlight = 3

// Legacy default amounts retained for callers. Runtime charges come from instance accounting.
const (
	MCPChargeEUR     = 0.01
	SearchChargeEUR  = 0.005
	FetchChargeEUR   = 0.002
	ToolboxChargeEUR = 0.02
)

// Budget is the per-person daily cap, shared by llm-gw and the manager's own
// calls on behalf of a person (Gateway.Complete).
type Budget struct {
	Accounting *instance.Accounting // nil uses the selected instance
	// Spent sums the person's cost since a time; CapEUR <= 0 disables the cap.
	Spent  func(ctx context.Context, uid string, since time.Time) (float64, error)
	CapEUR float64
	Now    func() time.Time

	mu   sync.Mutex
	held map[string]hold // calls in flight per uid
	ops  map[string]*sync.Mutex
}

type hold struct {
	n   int
	eur float64
}

// Reserve admits one LLM call of uid or returns ErrDayCap or ErrBusy.
// release must run once the call's usage is recorded.
func (b *Budget) Reserve(ctx context.Context, uid string) (release func(), err error) {
	a := instance.Current().Accounting
	if b != nil && b.Accounting != nil {
		a = *b.Accounting
	}
	return b.ReserveEUR(ctx, uid, a.ReserveEUR())
}

// ReserveEUR admits one call of uid with an estimated cost of eur.
// LLM calls, central MCP calls and tool runs share the
// day's cap and the maxInFlight places. release must run once the call's
// usage (with its charge) is recorded.
func (b *Budget) ReserveEUR(ctx context.Context, uid string, eur float64) (release func(), err error) {
	if b == nil || b.CapEUR <= 0 {
		return func() {}, nil
	}
	b.mu.Lock()
	if b.ops == nil {
		b.ops = map[string]*sync.Mutex{}
	}
	op := b.ops[uid]
	if op == nil {
		op = &sync.Mutex{}
		b.ops[uid] = op
	}
	b.mu.Unlock()
	op.Lock() // A slow spend lookup must only block this person's admissions.
	defer op.Unlock()
	b.mu.Lock()
	h := b.held[uid]
	b.mu.Unlock()
	if h.n >= maxInFlight {
		slog.DebugContext(ctx, "budget reserve", "uid", uid, "eur", eur, "in_flight", h.n, "held_eur", h.eur, "result", "busy")
		return nil, ErrBusy
	}
	spent, err := b.Spent(ctx, uid, config.DayStart(b.Now()))
	if err != nil {
		return nil, err
	}
	if spent+h.eur+1e-9 >= b.CapEUR { // float sums of euro amounts: 4.99 + 0.01 may land just below 5
		slog.DebugContext(ctx, "budget reserve", "uid", uid, "eur", eur, "spent_eur", spent, "held_eur", h.eur, "cap_eur", b.CapEUR, "result", "day cap")
		return nil, ErrDayCap
	}
	slog.DebugContext(ctx, "budget reserve", "uid", uid, "eur", eur, "spent_eur", spent, "held_eur", h.eur, "cap_eur", b.CapEUR,
		"in_flight", h.n+1, "result", "ok")
	b.mu.Lock()
	if b.held == nil {
		b.held = map[string]hold{}
	}
	b.held[uid] = hold{h.n + 1, h.eur + eur}
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			op.Lock() // Keep a completed charge's hold until the spend snapshot is consumed.
			defer op.Unlock()
			b.mu.Lock()
			h := b.held[uid]
			if h.n--; h.n <= 0 {
				delete(b.held, uid)
			} else {
				h.eur -= eur
				b.held[uid] = h
			}
			b.mu.Unlock()
			slog.DebugContext(ctx, "budget release", "uid", uid, "eur", eur, "in_flight", max(h.n, 0))
		})
	}, nil
}

// Refusal is the text for a refused ReserveEUR, as tool callers show it
// (German: no language is known here).
func Refusal(err error) string {
	switch {
	case errors.Is(err, ErrDayCap):
		return german(msgDayCap)
	case errors.Is(err, ErrBusy):
		return german(msgBusy)
	}
	return german(msgInternal)
}

func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	addr, _ := netip.ParseAddr(host)
	slot, ok := g.SlotOf(addr)
	if !ok {
		g.Next.ServeHTTP(w, r)
		return
	}
	ctx := logx.Set(r.Context(), logx.Fields{Slot: slot})
	u, err := g.Users.UserBySlot(ctx, slot)
	if err != nil {
		g.Next.ServeHTTP(w, r)
		return
	}
	logx.Set(ctx, logx.Fields{UID: u.UID})
	if g.RuntimeAccess != nil {
		if err := g.RuntimeAccess(ctx, u.UID); err != nil {
			oaiError(w, http.StatusForbidden, "Assistant runtime unavailable.", "permission_denied", false)
			return
		}
	}
	consented, err := g.Consented(ctx, u.UID)
	if err != nil {
		slog.ErrorContext(ctx, "llm-gw: consent lookup failed", "err", err)
		oaiError(w, http.StatusInternalServerError, german(msgInternal), "internal_error", true)
		return
	}
	if !consented {
		slog.InfoContext(ctx, "llm-gw: refused", "reason", "no consent", "path", r.URL.Path)
		oaiError(w, http.StatusForbidden, german("Please confirm the prototype notice in the portal first: %s", g.Portal+"/consent"), "permission_denied", false)
		return
	}
	if routes[r.Method+" "+r.URL.Path] != "" {
		release, err := g.Budget.Reserve(ctx, u.UID)
		if err != nil {
			slog.InfoContext(ctx, "llm-gw: refused", "reason", "budget", "path", r.URL.Path, "err", err)
		}
		switch {
		case errors.Is(err, ErrDayCap):
			oaiError(w, http.StatusTooManyRequests, german(msgDayCap), "insufficient_quota", false)
			return
		case errors.Is(err, ErrBusy):
			w.Header().Set("Retry-After", "2")
			oaiError(w, http.StatusTooManyRequests, german(msgBusy), "rate_limit_exceeded", true)
			return
		case err != nil:
			oaiError(w, http.StatusInternalServerError, german(msgInternal), "internal_error", true)
			return
		}
		// the proxy records the usage before it returns (meter closes the body)
		defer release()
	}
	g.Next.ServeHTTP(w, r)
}
