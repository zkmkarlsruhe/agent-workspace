// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// Parallel calls hold budget while in flight: at 4.80 of 5 EUR spent only
// one call fits, however many arrive at once; at most maxInFlight per person.
func TestBudgetReservesInFlight(t *testing.T) {
	ctx := context.Background()
	spent := 4.80
	b := &Budget{Spent: func(context.Context, string, time.Time) (float64, error) { return spent, nil }, CapEUR: 5, Now: time.Now}
	r1, err := b.Reserve(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve(ctx, "u1"); !errors.Is(err, ErrDayCap) {
		t.Fatalf("second call below the cap only by the first's hold: %v", err)
	}
	if _, err := b.Reserve(ctx, "u2"); err != nil {
		t.Fatalf("another person is not held back: %v", err)
	}
	r1()
	r1() // idempotent
	if r, err := b.Reserve(ctx, "u1"); err != nil {
		t.Fatalf("after release: %v", err)
	} else {
		r()
	}
	spent = 0
	var held []func()
	for range maxInFlight {
		r, err := b.Reserve(ctx, "u1")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, r)
	}
	if _, err := b.Reserve(ctx, "u1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrency limit: %v", err)
	}
	for _, r := range held {
		r()
	}
}

// Tool calls hold their own small charge against the same cap and places.
func TestBudgetReserveEUR(t *testing.T) {
	ctx := context.Background()
	b := &Budget{Spent: func(context.Context, string, time.Time) (float64, error) { return 4.98, nil }, CapEUR: 5, Now: time.Now}
	r1, err1 := b.ReserveEUR(ctx, "u1", MCPChargeEUR)
	r2, err2 := b.ReserveEUR(ctx, "u1", MCPChargeEUR)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if _, err := b.ReserveEUR(ctx, "u1", MCPChargeEUR); !errors.Is(err, ErrDayCap) {
		t.Fatalf("third call over the cap: %v", err)
	}
	r1()
	if _, err := b.Reserve(ctx, "u1"); err != nil { // 4.98 + 0.01 held: still below the cap
		t.Fatalf("llm call next to one tool call: %v", err)
	}
	r2()
}

// The gate holds the reservation until the proxied call returned.
func TestGateConcurrentCalls(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{}, 2*maxInFlight)
	g := &Gate{SlotOf: config.Config{UserNetPrefix: "10.90"}.SlotOf, Users: users{},
		Consented: func(context.Context, string) (bool, error) { return true, nil },
		Budget:    &Budget{Spent: func(context.Context, string, time.Time) (float64, error) { return 0, nil }, CapEUR: 5, Now: time.Now},
		Next:      http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-block })}
	do := func() int {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
		r.RemoteAddr = "10.90.1.10:1"
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w.Code
	}
	var wg sync.WaitGroup
	for range maxInFlight {
		wg.Go(func() { do() })
	}
	for range maxInFlight {
		<-entered
	}
	if code := do(); code != http.StatusTooManyRequests {
		t.Fatalf("call beyond the limit: %d", code)
	}
	close(block)
	wg.Wait()
	if code := do(); code != http.StatusOK {
		t.Fatalf("after the calls ended: %d", code)
	}
}

// Manager calls on behalf of a person go through the same budget.
func TestCompleteRespectsBudget(t *testing.T) {
	gw, _ := gateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	spent := 5.0
	gw.Budget = &Budget{Spent: func(context.Context, string, time.Time) (float64, error) { return spent, nil }, CapEUR: 5, Now: time.Now}
	if _, err := gw.Complete(context.Background(), store.User{UID: "u1"}, "s", "u", 10); !errors.Is(err, ErrDayCap) {
		t.Fatalf("person call over the cap: %v", err)
	}
	if out, err := gw.Complete(context.Background(), store.User{UID: store.ManagerUID}, "s", "u", 10); err != nil || out != "ok" {
		t.Fatalf("manager's own call: %q %v", out, err)
	}
	spent = 0
	if _, err := gw.Complete(context.Background(), store.User{UID: "u1"}, "s", "u", 10); err != nil {
		t.Fatal(err)
	}
}
