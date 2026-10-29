// SPDX-License-Identifier: MIT

package router

import (
	"context"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFreeCADRouting(t *testing.T) {
	hits := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		for _, h := range []string{"Cookie", "X-Authentik-Uid", "X-Authentik-Jwt"} {
			if r.Header.Get(h) != "" {
				t.Errorf("credential forwarded: %s", h)
			}
		}
		if user, password, ok := r.BasicAuth(); !ok || user != "workspace" || password != "worker-token" {
			t.Error("missing manager GUI credential")
		}
		if r.URL.Path != "/websocket" {
			t.Error("path lost")
		}
		w.Header().Set("Set-Cookie", "test=secret")
		w.Write([]byte("desktop"))
	}))
	defer backend.Close()
	rt, _ := newRouter(backend.URL, true)
	session := strings.Repeat("a", 32)
	host := "https://cad-" + session + ".apps.example.org"
	rt.FreeCADTarget = func(ctx context.Context, uid, id string) (lifecycle.Target, bool) {
		return lifecycle.Target{BaseURL: backend.URL, Password: "worker-token"}, uid == uidAlice && id == session
	}
	for _, test := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"owner", func(r *http.Request) { r.Header.Set("Authorization", "Bearer private") }, 200},
		{"foreign", func(r *http.Request) { r.Header.Set("X-Authentik-Uid", uidOther) }, 404},
		{"unknown session", func(r *http.Request) { r.Host = strings.Replace(r.Host, session, strings.Repeat("b", 32), 1) }, 404},
		{"foreign origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403},
		{"missing WS origin", func(r *http.Request) { r.Header.Set("Upgrade", "websocket") }, 403},
		{"untrusted", func(r *http.Request) { r.RemoteAddr = "10.90.1.12:5000" }, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := authed("GET", host+"/websocket")
			test.change(r)
			w := httptest.NewRecorder()
			rt.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if w.Header().Get("Set-Cookie") != "" {
				t.Fatal("upstream cookie escaped")
			}
		})
	}
	if hits != 1 {
		t.Fatal("unauthorized request reached app", hits)
	}
}

func TestFreeCADStreamRevocation(t *testing.T) {
	entered := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
	}))
	defer backend.Close()
	rt, _ := newRouter(backend.URL, true)
	var allowed atomic.Bool
	allowed.Store(true)
	rt.FreeCADTarget = func(context.Context, string, string) (lifecycle.Target, bool) {
		return lifecycle.Target{BaseURL: backend.URL}, allowed.Load()
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		rt.ServeHTTP(httptest.NewRecorder(), authed("GET", "https://cad-"+strings.Repeat("a", 32)+".apps.example.org/stream"))
	}()
	<-entered
	allowed.Store(false)
	select {
	case <-finished:
	case <-time.After(7 * time.Second):
		t.Fatal("revoked stream remains connected")
	}
}

func TestFreeCADProxyInjectsWorkerAuth(t *testing.T) {
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	calls := 0
	http.DefaultTransport = assistantTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		user, password, ok := r.BasicAuth()
		if !ok || user != "workspace" || password != "exec-token" {
			t.Error("worker auth missing")
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("browser cookie forwarded")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("desktop"))}, nil
	})
	rt, _ := newRouter("http://cad", true)
	rt.FreeCADTarget = func(context.Context, string, string) (lifecycle.Target, bool) {
		return lifecycle.Target{BaseURL: "http://cad", Password: "exec-token"}, true
	}
	req := authed("GET", "https://cad-"+strings.Repeat("a", 32)+".apps.example.org/")
	req.Header.Set("Authorization", "Bearer browser-secret")
	req.Header.Set("Cookie", "browser=secret")
	out := httptest.NewRecorder()
	rt.ServeHTTP(out, req)
	if calls != 1 || out.Code != 200 {
		t.Fatal("GUI request not proxied", out.Code)
	}
	if strings.Contains(out.Body.String(), "exec-token") {
		t.Fatal("worker token leaked to browser")
	}
}
