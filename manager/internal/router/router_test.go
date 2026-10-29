// SPDX-License-Identifier: MIT

package router

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

const (
	uidAlice = "a7889ac42c1d44753644ec24acde57e8650e05b30d4b86f39dc0c96a98d25356"
	uidOther = "288992ff55666970b6ddc030565d3c4c9452a9543cc3718d1b1ad5e52523ece4"
)

type users map[string]string // uid -> slug

func (u users) CheckIdentity(_ context.Context, id identity.Identity) (string, error) {
	return id.UID, nil
}

func (u users) EnsureUser(_ context.Context, id identity.Identity) (store.User, error) {
	return store.User{UID: id.UID, Slug: u[id.UID]}, nil
}

type fakeLC struct {
	mu      sync.Mutex
	target  string
	running bool
	streams int
	touched int
}

func (f *fakeLC) Target(string) (lifecycle.Target, bool) {
	return lifecycle.Target{BaseURL: f.target, Password: "pw"}, f.running
}
func (f *fakeLC) Touch(string) { f.mu.Lock(); f.touched++; f.mu.Unlock() }
func (f *fakeLC) StreamOpened(string) func() {
	f.mu.Lock()
	f.streams++
	f.mu.Unlock()
	return func() { f.mu.Lock(); f.streams--; f.mu.Unlock() }
}
func (f *fakeLC) Suspect(string) {}

func newRouter(backend string, running bool) (*Router, *fakeLC) {
	lc := &fakeLC{target: backend, running: running}
	return New(Router{
		Domain:  "apps.example.org",
		Checker: identity.Checker{Trusted: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, RequiredGroup: "opencode-users"},
		Users:   users{uidAlice: "alice", uidOther: "anna"},
		LC:      lc,
		Portal: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, _ := identity.FromContext(r.Context())
			fmt.Fprint(w, "portal:"+id.Slug)
		}),
		Consented: func(context.Context, string) (bool, error) { return true, nil },
		Log:       slog.New(slog.DiscardHandler),
	}), lc
}

func authed(method, url string) *http.Request {
	r := httptest.NewRequest(method, url, nil)
	r.RemoteAddr = "192.0.2.10:5555"
	r.Header.Set("X-Authentik-Uid", uidAlice)
	r.Header.Set("X-Authentik-Username", "alice")
	r.Header.Set("X-Authentik-Groups", "opencode-users")
	r.Header.Set("X-Authentik-Jwt", "secret")
	r.Header.Set("Cookie", "authentik_proxy_x=1")
	return r
}

func TestRules(t *testing.T) {
	var got *http.Request
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	rt, lc := newRouter(backend.URL, true)

	do := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		return w
	}
	r := authed("GET", "https://alice.apps.example.org/")
	r.RemoteAddr = "10.90.1.10:1"
	if w := do(r); w.Code != 403 {
		t.Fatalf("untrusted source: %d", w.Code)
	}
	if w := do(authed("GET", "https://anna.apps.example.org/")); w.Code != 302 || w.Header().Get("Location") != "https://apps.example.org/" {
		t.Fatalf("foreign slug: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := do(authed("GET", "https://nobody.apps.example.org/")); w.Code != 302 {
		t.Fatalf("unknown slug: %d", w.Code)
	}
	r = authed("GET", "https://anna.apps.example.org/api/pty/1/connect")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Origin", "https://anna.apps.example.org")
	if w := do(r); w.Code != 403 {
		t.Fatalf("foreign WS: %d", w.Code)
	}
	r = authed("POST", "https://alice.apps.example.org/api/session")
	r.Header.Set("Origin", "https://evil.example")
	if w := do(r); w.Code != 403 {
		t.Fatalf("cross-origin write: %d", w.Code)
	}
	r = authed("GET", "https://alice.apps.example.org/api/pty/1/connect")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Origin", "https://anna.apps.example.org")
	if w := do(r); w.Code != 403 {
		t.Fatalf("WS with foreign origin: %d", w.Code)
	}
	if w := do(authed("POST", "https://alice.apps.example.org/api/pair")); w.Code != 404 {
		t.Fatalf("pairing: %d", w.Code)
	}
	if w := do(authed("GET", "https://apps.example.org/")); w.Body.String() != "portal:alice" {
		t.Fatalf("portal: %q", w.Body.String())
	}

	r = authed("POST", "https://alice.apps.example.org/api/session?x=1")
	r.Header.Set("Origin", "https://alice.apps.example.org")
	r.Header.Set("Authorization", "Basic Zm9vOmJhcg==")
	if w := do(r); w.Code != 200 {
		t.Fatalf("proxy: %d", w.Code)
	}
	if got.Host != "alice.apps.example.org" || got.URL.RawQuery != "x=1" {
		t.Fatalf("host/query not preserved: %s %s", got.Host, got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("opencode:pw")) {
		t.Fatal("basic not injected")
	}
	if got.Header.Get("Cookie") != "" || got.Header.Get("X-Authentik-Uid") != "" || got.Header.Get("X-Authentik-Jwt") != "" {
		t.Fatal("identity headers leaked")
	}
	if got.Header.Get("X-Forwarded-Proto") != "https" || lc.touched != 1 {
		t.Fatal("forwarded/touch")
	}

	rt2, _ := newRouter(backend.URL, false)
	w := httptest.NewRecorder()
	r = authed("GET", "https://alice.apps.example.org/")
	r.Header.Set("Accept", "text/html")
	rt2.ServeHTTP(w, r)
	if w.Code != 302 || w.Header().Get("Location") != "https://apps.example.org/environment" {
		t.Fatalf("not running nav: %d %s", w.Code, w.Header().Get("Location"))
	}
	w = httptest.NewRecorder()
	rt2.ServeHTTP(w, authed("GET", "https://alice.apps.example.org/api/event"))
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("not running SSE: %d", w.Code)
	}
}

// serve runs the router on a real listener; requests appear from 127.0.0.1,
// so the trusted prefix is widened to loopback.
func serve(t *testing.T, rt *Router) string {
	rt.Checker.Trusted = append(rt.Checker.Trusted, netip.MustParsePrefix("127.0.0.0/8"))
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestSSEUnbuffered(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer backend.Close()
	defer close(release)
	rt, lc := newRouter(backend.URL, true)
	addr := serve(t, rt)
	req, _ := http.NewRequest("GET", "http://"+addr+"/api/event", nil)
	req.Host = "alice.apps.example.org"
	for k, v := range authed("GET", "/").Header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first event not delivered before stream end: %q %v", line, err)
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.streams != 1 {
		t.Fatalf("open stream not counted: %d", lc.streams)
	}
}

func TestWebSocketUpgrade(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" || r.Host != "alice.apps.example.org" {
			w.WriteHeader(400)
			return
		}
		conn, buf, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		buf.Flush()
		line, _ := buf.ReadString('\n')
		buf.WriteString("echo " + line)
		buf.Flush()
	}))
	defer backend.Close()
	rt, _ := newRouter(backend.URL, true)
	addr := serve(t, rt)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /api/pty/1/connect HTTP/1.1\r\nHost: alice.apps.example.org\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Origin: https://alice.apps.example.org\r\nX-Authentik-Uid: %s\r\nX-Authentik-Username: alice\r\nX-Authentik-Groups: opencode-users\r\n\r\n", uidAlice)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	io.WriteString(conn, "hallo\n")
	if line, _ := br.ReadString('\n'); strings.TrimSpace(line) != "echo hallo" {
		t.Fatalf("ws data: %q", line)
	}
}

type blockedUsers struct{ users }

func (blockedUsers) CheckIdentity(context.Context, identity.Identity) (string, error) {
	return "", &store.IdentityBlockedError{StoredHash: identity.SubjectHash("issuer", uidAlice), IncomingHash: identity.SubjectHash("issuer", uidOther)}
}

func TestBlockedIdentityLogsHashes(t *testing.T) {
	rt, lc := newRouter("http://unused", true)
	rt.Users = blockedUsers{}
	var logs strings.Builder
	rt.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	rt.Portal = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := identity.FromContext(r.Context())
		if !id.Blocked || id.UID != "" || id.Username != "" {
			t.Errorf("blocked identity passed account data: %+v", id)
		}
		w.WriteHeader(http.StatusForbidden)
	})
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, authed("GET", "https://alice.apps.example.org/api/info"))
	if w.Code != http.StatusForbidden || lc.touched != 0 {
		t.Fatalf("blocked request reached runtime: %d", w.Code)
	}
	log := logs.String()
	if strings.Contains(log, uidAlice) || strings.Contains(log, uidOther) || !strings.Contains(log, `"level":"ERROR"`) ||
		!strings.Contains(log, identity.SubjectHash("issuer", uidAlice)) || !strings.Contains(log, identity.SubjectHash("issuer", uidOther)) {
		t.Fatalf("unsafe or missing identity log: %s", log)
	}
}

func TestCustomIdentityHeadersStripped(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subject") != "" {
			t.Error("custom identity headers reached runtime")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	rt, _ := newRouter(backend.URL, true)
	rt.Checker.Issuer, rt.Checker.SubjectHeader = "fixed-issuer", "X-Subject"
	r := authed("GET", "https://alice.apps.example.org/api/info")
	r.Header.Set("X-Subject", "subject")
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("custom identity refused: %d", w.Code)
	}
}

func TestMappedIdentityHeadersStripped(t *testing.T) {
	rt, _ := newRouter("http://runtime:4096", true)
	rt.Checker.Headers = identity.Headers{UID: "X-User", Username: "X-Login", Email: "X-Mail", Name: "X-Name", Groups: "X-Groups"}
	rt.Checker.SubjectHeader = "X-Subject"
	r := authed("GET", "https://alice.apps.example.org/api/info")
	for _, name := range []string{"X-User", "X-Login", "X-Mail", "X-Name", "X-Groups", "X-Subject"} {
		r.Header.Set(name, "private")
	}
	r.Header["x-authentik-extra"] = []string{"private"}
	r.Header.Set("X-Keep", "public")
	r = r.WithContext(context.WithValue(r.Context(), proxyKey{}, proxyTarget{target: lifecycle.Target{BaseURL: "http://runtime:4096", Password: "pw"}}))
	pr := &httputil.ProxyRequest{In: r, Out: r.Clone(r.Context())}
	rt.proxy.Rewrite(pr)
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-authentik-") || name == "Cookie" || strings.Contains("|X-User|X-Login|X-Mail|X-Name|X-Groups|X-Subject|", "|"+name+"|") {
			if len(pr.Out.Header.Values(name)) != 0 {
				t.Errorf("header leaked: %s", name)
			}
		}
	}
	if pr.Out.Header.Get("X-Keep") != "public" {
		t.Fatal("unrelated header stripped")
	}
	if user, pass, ok := pr.Out.BasicAuth(); !ok || user != "opencode" || pass != "pw" {
		t.Fatal("runtime auth missing")
	}
}
