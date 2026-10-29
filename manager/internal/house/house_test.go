// SPDX-License-Identifier: MIT

package house

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

const uid1 = "a7889ac42c1d44753644ec24acde57e8650e05b30d4b86f39dc0c96a98d25356"

// fakeNC is a Nextcloud with Login Flow v2, the OCS user and app password
// endpoints, and WebDAV PROPFIND on the root. Grant() lets the next poll
// succeed for the given account.
type fakeNC struct {
	*httptest.Server
	mu       sync.Mutex
	valid    map[string]string // app password -> login name
	users    map[string]string // login name -> user id
	pending  *grant
	served   int // successful polls
	revoked  []string
	server   string // "server" in the grant; "" = own URL
	n        int
	noRevoke bool // DELETE apppassword answers 503
}

func newFakeNC(t *testing.T) *fakeNC {
	f := &fakeNC{valid: map[string]string{}, users: map[string]string{"alice": "alice", "Kollege": "kollege", "alice@example.org": "alice"}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeNC) grant(login string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	pw := "app-pw-" + login + "-" + string(rune('a'+f.n))
	server := f.server
	if server == "" {
		server = f.URL
	}
	f.pending = &grant{Server: server, LoginName: login, AppPassword: pw}
	return pw
}

func (f *fakeNC) isValid(pw string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.valid[pw]
	return ok
}

func (f *fakeNC) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/index.php/login/v2":
		json.NewEncoder(w).Encode(map[string]any{"poll": map[string]string{"token": "tok", "endpoint": f.URL + "/login/v2/poll"},
			"login": f.URL + "/login/v2/flow/abc"})
		return
	case r.Method == "POST" && r.URL.Path == "/login/v2/poll":
		if r.FormValue("token") != "tok" || f.pending == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("[]"))
			return
		}
		g := *f.pending
		f.pending = nil
		f.valid[g.AppPassword] = g.LoginName
		f.served++
		json.NewEncoder(w).Encode(g)
		return
	}
	login, pw, _ := r.BasicAuth()
	if f.valid[pw] != login || login == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/ocs/v2.php/cloud/user" && r.Header.Get("OCS-APIRequest") == "true":
		json.NewEncoder(w).Encode(map[string]any{"ocs": map[string]any{"data": map[string]string{"id": f.users[login]}}})
	case r.Method == "DELETE" && r.URL.Path == "/ocs/v2.php/core/apppassword" && r.Header.Get("OCS-APIRequest") == "true":
		if f.noRevoke {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		delete(f.valid, pw)
		f.revoked = append(f.revoked, pw)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func setup(t *testing.T) (*Service, *fakeNC, *store.Store) {
	t.Helper()
	nc := newFakeNC(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.EnsureUser(context.Background(), identity.Identity{UID: uid1, Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(nc.URL)
	s, err := New([]config.HouseService{{ID: "nextcloud", Title: "Nextcloud", URL: u}}, st, []byte("0123456789abcdef0123456789abcdef"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s.PollEvery = 5 * time.Millisecond
	return s, nc, st
}

// wait polls until the flow left FlowPending.
func wait(t *testing.T, s *Service) FlowState {
	t.Helper()
	for range 400 {
		if f := s.flow(uid1, "nextcloud"); f.State != FlowPending {
			return f.State
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("flow still pending")
	return 0
}

func connect(t *testing.T, s *Service, nc *fakeNC, login string) (string, FlowState) {
	t.Helper()
	link, err := s.Connect(context.Background(), uid1, "alice", "nextcloud")
	if err != nil || link != nc.URL+"/login/v2/flow/abc" {
		t.Fatalf("connect: %q %v", link, err)
	}
	if f := s.flow(uid1, "nextcloud"); f.State != FlowPending || f.LoginURL != link {
		t.Fatalf("flow %+v", f)
	}
	time.Sleep(20 * time.Millisecond) // a few 404 polls first
	pw := nc.grant(login)
	return pw, wait(t, s)
}

func TestConnectStoresEncrypted(t *testing.T) {
	s, nc, st := setup(t)
	ctx := context.Background()
	if _, _, _, err := s.Creds(ctx, uid1, "nextcloud"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("before connect: %v", err)
	}
	pw, state := connect(t, s, nc, "alice@example.org") // e-mail login, user id alice
	if state != FlowDone {
		t.Fatalf("state %v", state)
	}
	user, login, got, err := s.Creds(ctx, uid1, "nextcloud")
	if err != nil || user != "alice" || login != "alice@example.org" || got != pw {
		t.Fatalf("creds %q %q %v", user, login, err)
	}
	c, _ := st.Connection(ctx, uid1, "nextcloud")
	if bytes.Contains(c.SecretEnc, []byte(pw)) || c.Login != "alice" {
		t.Fatal("secret stored in plain text")
	}
	// bound to its row: the same blob does not open for another uid, nor with another key
	if _, err := open(s.key, aad(strings.Repeat("b", 64), "nextcloud"), c.SecretEnc); err == nil {
		t.Fatal("blob opened for another person")
	}
	k2, _ := deriveKey([]byte("another master key, 32 bytes or more"))
	if _, err := open(k2, aad(uid1, "nextcloud"), c.SecretEnc); err == nil {
		t.Fatal("blob opened with another key")
	}
	sts, _ := s.Status(ctx, uid1)
	if len(sts) != 1 || !sts[0].Connected || sts[0].Login != "alice" || sts[0].Expired || sts[0].Flow.State != FlowDone {
		t.Fatalf("status %+v", sts)
	}
}

func TestConnectOtherAccountRevoked(t *testing.T) {
	s, nc, st := setup(t)
	pw, state := connect(t, s, nc, "Kollege")
	if state != FlowMismatch || nc.isValid(pw) {
		t.Fatalf("state %v, still valid %v", state, nc.isValid(pw))
	}
	if _, err := st.Connection(context.Background(), uid1, "nextcloud"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("foreign account stored")
	}
	// mapping hook: alice may connect the account "Kollege"
	s.LoginFor = func(string) string { return "kollege" }
	if _, state = connect(t, s, nc, "Kollege"); state != FlowDone {
		t.Fatalf("mapped: %v", state)
	}
}

func TestConnectOtherServerRevoked(t *testing.T) {
	s, nc, _ := setup(t)
	nc.server = "https://evil.example"
	pw, state := connect(t, s, nc, "alice")
	if state != FlowMismatch || nc.isValid(pw) {
		t.Fatalf("state %v", state)
	}
}

func TestConnectTimeout(t *testing.T) {
	s, _, _ := setup(t)
	s.flowTTL = 30 * time.Millisecond
	if _, err := s.Connect(context.Background(), uid1, "alice", "nextcloud"); err != nil {
		t.Fatal(err)
	}
	if st := wait(t, s); st != FlowTimeout {
		t.Fatalf("state %v", st)
	}
}

func TestReconnectRevokesOld(t *testing.T) {
	s, nc, _ := setup(t)
	ctx := context.Background()
	old, _ := connect(t, s, nc, "alice")
	newPW, state := connect(t, s, nc, "alice")
	if state != FlowDone || nc.isValid(old) || !nc.isValid(newPW) {
		t.Fatalf("state %v old valid %v new valid %v", state, nc.isValid(old), nc.isValid(newPW))
	}
	if _, _, got, _ := s.Creds(ctx, uid1, "nextcloud"); got != newPW {
		t.Fatal("new password not stored")
	}
}

func TestExpiredAndDisconnect(t *testing.T) {
	s, nc, st := setup(t)
	ctx := context.Background()
	connect(t, s, nc, "alice")
	s.MarkExpired(ctx, uid1, "nextcloud")
	if _, _, _, err := s.Creds(ctx, uid1, "nextcloud"); !errors.Is(err, ErrNotConnected) {
		t.Fatal("expired connection handed out")
	}
	if sts, _ := s.Status(ctx, uid1); !sts[0].Expired || !sts[0].Connected {
		t.Fatalf("status %+v", sts)
	}
	// Nextcloud down: the row stays so Disconnect can revoke later
	nc.Close()
	if err := s.Disconnect(ctx, uid1, "nextcloud"); err == nil {
		t.Fatal("disconnect without revoke")
	}
	if _, err := st.Connection(ctx, uid1, "nextcloud"); err != nil {
		t.Fatal("row lost while Nextcloud was down")
	}
}

func TestDisconnectAllRevokes(t *testing.T) {
	s, nc, st := setup(t)
	ctx := context.Background()
	pw, _ := connect(t, s, nc, "alice")
	if err := s.DisconnectAll(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if nc.isValid(pw) || len(nc.revoked) != 1 {
		t.Fatal("not revoked")
	}
	if _, err := st.Connection(ctx, uid1, "nextcloud"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("row kept")
	}
	// already revoked in Nextcloud (401) counts as revoked
	pw, _ = connect(t, s, nc, "alice")
	nc.mu.Lock()
	delete(nc.valid, pw)
	nc.mu.Unlock()
	if err := s.Disconnect(ctx, uid1, "nextcloud"); err != nil {
		t.Fatal(err)
	}
	if err := s.Disconnect(ctx, uid1, "nextcloud"); err != nil {
		t.Fatal("second disconnect", err)
	}
	if err := s.Disconnect(ctx, uid1, "gibtsnicht"); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
}

func TestConnectRefusesForeignLoginURL(t *testing.T) {
	s, _, _ := setup(t)
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"poll": map[string]string{"token": "t", "endpoint": "https://evil.example/poll"}, "login": "https://evil.example/login"})
	}))
	defer evil.Close()
	s.Services[0].URL, _ = url.Parse(evil.URL)
	if _, err := s.Connect(context.Background(), uid1, "alice", "nextcloud"); !errors.Is(err, ErrUnreachable) {
		t.Fatal(err)
	}
	if _, err := s.Connect(context.Background(), uid1, "alice", "outline"); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
}
