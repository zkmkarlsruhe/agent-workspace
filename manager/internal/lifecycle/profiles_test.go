// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle/lctest"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestAssistantStagingStopsBusySharedRuntime(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.EnsureUser(context.Background(), identity.Identity{UID: uid1, Username: "test"}); err != nil {
		t.Fatal(err)
	}
	oc := &lctest.OpenCode{}
	rt := lctest.NewRuntime()
	svc := New(config.Config{UserNetPrefix: "10.90", StartTimeout: time.Second}, rt, st, slog.New(slog.DiscardHandler))
	svc.http.Transport = assistantTransport{oc}
	e := env{svc, rt, oc, st}
	ctx := context.Background()
	e.rt.OpenCode = e.oc
	for _, folder := range []string{"support", "ordinary"} {
		if err := e.st.CreateProject(ctx, store.Project{ID: folder, UID: uid1, Folder: folder, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	target, ok := e.s.Target(uid1)
	if !ok {
		t.Fatal("no target")
	}
	e.oc.Mu.Lock()
	e.oc.Active = []string{"ses_root", "ses_child"}
	e.oc.Mu.Unlock()
	if err := e.s.BindProjectAssistant(ctx, uid1, "support", "missing"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, ok := e.s.Target(uid1); !ok {
		t.Fatal("invalid profile stopped runtime")
	}
	if err := e.s.BindProjectAssistant(ctx, uid1, "support", "it-hilfe"); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.s.Target(uid1); ok {
		t.Fatal("shared target remains")
	}
	if _, release, ok := target.Bind(ctx); ok {
		release()
		t.Fatal("old generation remains")
	}
	e.oc.Mu.Lock()
	active := len(e.oc.Active)
	e.oc.Mu.Unlock()
	if active != 0 {
		t.Fatal("root or child still running")
	}
	if _, err := e.s.EnsureRunning(ctx, uid1); !errors.Is(err, assistants.ErrIsolationRequired) {
		t.Fatal(err)
	}
	p, err := e.st.ProjectByFolder(ctx, uid1, "support")
	if err != nil || p.Assistant.ID != "it-hilfe" || p.Assistant.Hash == "" {
		t.Fatalf("pin: %+v %v", p, err)
	}
	// No switch to another project or default agent can restart this shared runtime.
	if err := e.s.BindProjectAssistant(ctx, uid1, "ordinary", "it-hilfe"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.EnsureRunning(ctx, uid1); !errors.Is(err, assistants.ErrIsolationRequired) {
		t.Fatal(err)
	}
}

// Exercise the same lctest HTTP handler without a listening socket.
type assistantTransport struct{ http.Handler }

func (p assistantTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w.Result(), nil
}
