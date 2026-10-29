// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/house"
)

// ncServer is a Nextcloud with Login Flow v2, OCS user/app password and a
// WebDAV PROPFIND of the user root. grant lets the next poll succeed.
type ncServer struct {
	*httptest.Server
	mu      sync.Mutex
	valid   map[string]string // app password -> login name
	pending map[string]string
	n       int
}

func newNCServer(t *testing.T) *ncServer {
	f := &ncServer{valid: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/index.php/login/v2":
			json.NewEncoder(w).Encode(map[string]any{"poll": map[string]string{"token": "tok", "endpoint": f.URL + "/login/v2/poll"}, "login": f.URL + "/login/v2/flow/x"})
			return
		case r.Method == "POST" && r.URL.Path == "/login/v2/poll":
			if f.pending == nil {
				http.Error(w, "[]", http.StatusNotFound)
				return
			}
			f.valid[f.pending["appPassword"]] = f.pending["loginName"]
			json.NewEncoder(w).Encode(f.pending)
			f.pending = nil
			return
		}
		login, pw, _ := r.BasicAuth()
		if login == "" || f.valid[pw] != login {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/ocs/v2.php/cloud/user":
			json.NewEncoder(w).Encode(map[string]any{"ocs": map[string]any{"data": map[string]string{"id": login}}})
		case r.Method == "DELETE" && r.URL.Path == "/ocs/v2.php/core/apppassword":
			delete(f.valid, pw)
		case r.Method == "PROPFIND" && r.URL.Path == "/remote.php/dav/files/"+login:
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprintf(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>/remote.php/dav/files/%s/</d:href></d:response>`+
				`<d:response><d:href>/remote.php/dav/files/%s/Leihvertraege.pdf</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>10</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`, login, login)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *ncServer) grant(login string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	pw := fmt.Sprintf("pw-%s-%d", login, f.n)
	f.pending = map[string]string{"server": f.URL, "loginName": login, "appPassword": pw}
	return pw
}

func (f *ncServer) isValid(pw string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.valid[pw]
	return ok
}

// The whole Connections flow on the full stack: first visit to the
// Onboarding, connect over Login Flow v2, tool call with the stored login,
// reconnect revoking the old app password, 401 shown as abgelaufen, another
// account refused and revoked, account deletion revoking the last one.
func TestConnectionsE2E(t *testing.T) {
	s := newStack(t, "127.84")
	nc := newNCServer(t)
	u, _ := url.Parse(nc.URL)
	hcfg := []config.HouseService{{ID: "nextcloud", Title: "Nextcloud", URL: u}}
	log := slog.New(slog.DiscardHandler)
	hs, err := house.New(hcfg, s.st, s.cfg.MasterKey, log)
	if err != nil {
		t.Fatal(err)
	}
	hs.PollEvery = 5 * time.Millisecond
	portalConns, portalOnboarding, s.svc.Disconnect = conns{hs}, onboarding{s.st}, hs.DisconnectAll
	t.Cleanup(func() { portalConns, portalOnboarding = nil, nil })
	s.buildEdge(t)
	s.svc.PurgeData = func(context.Context, string, int) error { return nil }
	workspace := mcp.New(mcp.Server{SearXNG: &url.URL{}, SlotOf: s.cfg.SlotOf, Users: s.st, Usage: s.st, Log: log,
		Nextcloud: ncTools(hs, hcfg[0], s.svc)})
	list := func() string {
		r := httptest.NewRequest("POST", "http://llm-gw:4000/mcp/workspace", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nextcloud_list","arguments":{}}}`))
		r.RemoteAddr = s.cfg.SlotAddr(1, config.HostUser).String() + ":40000"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		workspace.ServeHTTP(w, r)
		return w.Body.String()
	}
	me := &alice

	wantLoc(t, s.do(t, call{url: "/", as: me}), "/onboarding")
	want(t, s.do(t, call{url: "/", as: me}), http.StatusOK)
	s.waitRunning(t)
	want(t, s.do(t, call{url: "/onboarding?step=2", as: me}), http.StatusOK, "Nextcloud", "nicht verbunden")
	if !strings.Contains(list(), "Nextcloud ist nicht verbunden") {
		t.Fatal("tool before connect")
	}

	connect := func(login string) (string, string) {
		t.Helper()
		wantLoc(t, s.do(t, call{method: "POST", url: "/connections/nextcloud/connect", form: url.Values{}, as: me}), "/connections/nextcloud")
		want(t, s.do(t, call{url: "/connections/nextcloud", as: me}), http.StatusOK, nc.URL+"/login/v2/flow/x", `hx-trigger="every 2s"`)
		pw := nc.grant(login)
		for range 200 {
			if body := s.do(t, call{url: "/connections/nextcloud", as: me}).Body.String(); !strings.Contains(body, "hx-trigger") {
				return pw, body
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("flow did not finish")
		return "", ""
	}
	first, body := connect("alice")
	if !strings.Contains(body, "Verbunden als <strong>alice</strong>") {
		t.Fatal(body)
	}
	if out := list(); !strings.Contains(out, "Leihvertraege.pdf") {
		t.Fatalf("tool with stored login: %s", out)
	}

	second, _ := connect("alice")
	if nc.isValid(first) || !nc.isValid(second) {
		t.Fatal("reconnect must revoke the previous app password")
	}

	// revoked in Nextcloud by the person: the next tool call reports it, the portal shows abgelaufen
	nc.mu.Lock()
	delete(nc.valid, second)
	nc.mu.Unlock()
	if out := list(); !strings.Contains(out, "unter Verbindungen neu verbinden") {
		t.Fatal(out)
	}
	want(t, s.do(t, call{url: "/connections", as: me}), http.StatusOK, "Verbindung abgelaufen", "Neu verbinden")
	if !strings.Contains(list(), "Nextcloud ist nicht verbunden") {
		t.Fatal("expired login must not be sent again")
	}

	other, body := connect("kollege")
	if !strings.Contains(body, "Bitte mit deinem eigenen Nextcloud-Konto verbinden.") || nc.isValid(other) {
		t.Fatalf("other account: valid %v\n%s", nc.isValid(other), body)
	}

	last, _ := connect("ALICE") // case-insensitive
	if !nc.isValid(last) {
		t.Fatal("not connected")
	}
	wantLoc(t, s.do(t, call{method: "POST", url: "/environment/delete", form: url.Values{"confirmation": {"LÖSCHEN"}}, as: me}), "https://alice.apps.example.org/__workspace/clear")
	nc.mu.Lock()
	left := len(nc.valid)
	nc.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d app passwords left after account deletion", left)
	}
}
