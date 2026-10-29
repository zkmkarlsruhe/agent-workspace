// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A slot container that carries another person (slot reused) never gets the exec.
func TestShRefusesForeignContainer(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	e.rt.Mu.Lock()
	e.rt.Ctrs["oc-user1"].Labels = map[string]string{"oc.uid": uid2, "oc.generation": "1"}
	e.rt.Mu.Unlock()
	if err := e.s.sh(ctx, uid1, "echo hi", nil, io.Discard); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("exec into a foreign container: %v", err)
	}
	e.rt.Mu.Lock()
	e.rt.Ctrs["oc-user1"].Labels = map[string]string{"oc.uid": uid1, "oc.generation": "7"}
	e.rt.Mu.Unlock()
	if err := e.s.sh(ctx, uid1, "echo hi", nil, io.Discard); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("exec into another generation: %v", err)
	}
}

// DeleteAccount frees the slot only after running execs ended.
func TestDeleteAccountDrainsExecs(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.s.PurgeData = func(context.Context, string, int) error { return nil }
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	started, once := make(chan struct{}), sync.Once{}
	opDone := make(chan error, 1)
	go func() {
		opDone <- e.s.sh(ctx, uid1, "echo go; cat >/dev/null", pr, writerFunc(func(p []byte) (int, error) {
			once.Do(func() { close(started) })
			return len(p), nil
		}))
	}()
	<-started
	delDone := make(chan error, 1)
	go func() { _, err := e.s.DeleteAccount(ctx, uid1); delDone <- err }()
	select {
	case <-delDone:
		t.Fatal("account deleted while an exec still ran")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := e.st.UserByUID(ctx, uid1); err != nil {
		t.Fatal("slot freed while an exec still ran")
	}
	pw.Close()
	if err := <-opDone; err != nil {
		t.Fatal(err)
	}
	if err := <-delDone; err != nil {
		t.Fatal(err)
	}
	if err := e.s.sh(ctx, uid1, "true", strings.NewReader(""), io.Discard); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("exec after deletion: %v", err)
	}
}

// DeleteAccount cancels and drains HTTP calls of the old generation before
// the slot is freed; a client or proxy target captured before never sends
// the old password to the slot address again (the next tenant's container).
func TestDeleteAccountCancelsClients(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.s.PurgeData = func(context.Context, string, int) error { return nil }
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	hits := 0
	hang := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		if r.URL.Path == "/api/mcp" { // waitMCP polling during CreateSession
			hang <- struct{}{}
			<-r.Context().Done()
			return
		}
		e.oc.ServeHTTP(w, r)
	}))
	defer srv.Close()
	e.s.baseURL = func(int) string { return srv.URL }
	c, _ := e.s.Client(uid1)
	tg, _ := e.s.Target(uid1)
	callDone := make(chan error, 1)
	go func() { _, err := c.MCPConnected(ctx, "/workspace/p", "workspace"); callDone <- err }()
	<-hang
	delDone := make(chan error, 1)
	go func() { _, err := e.s.DeleteAccount(ctx, uid1); delDone <- err }()
	select {
	case err := <-delDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deletion waits for a hanging call")
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("call of the deleted generation succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("call of the deleted generation still runs")
	}
	mu.Lock()
	before := hits
	mu.Unlock()
	if _, err := c.Info(ctx); err == nil {
		t.Fatal("captured client still works")
	}
	if _, _, ok := tg.Bind(ctx); ok {
		t.Fatal("captured proxy target still binds")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != before {
		t.Fatal("request of the deleted generation reached the slot address")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
