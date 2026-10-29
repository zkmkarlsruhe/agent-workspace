// SPDX-License-Identifier: MIT

//go:build portalharness

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/gateway/mcp"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/house"
)

// Connections against a real Nextcloud:
//
//	OC_HARNESS_NC_URL=https://cloud.example.org OC_HARNESS_USER=<nextcloud login> \
//	  go test -tags portalharness -run TestHarnessNC -timeout 0 ./cmd/manager
//
// Portal on http://127.0.0.1:47822 as that user (fresh account: the first
// visit shows the Onboarding). Probe on http://127.0.0.1:47823:
// /liste runs the tool nextcloud_list with the stored login, /probe sends
// one WebDAV PROPFIND per app password the harness has seen so far and
// prints only the status codes (never a password).
func TestHarnessNC(t *testing.T) {
	raw, user := os.Getenv("OC_HARNESS_NC_URL"), os.Getenv("OC_HARNESS_USER")
	if raw == "" || user == "" {
		t.Skip("OC_HARNESS_NC_URL and OC_HARNESS_USER needed")
	}
	s := newStack(t, "127.86")
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		t.Fatal(err)
	}
	hcfg := []config.HouseService{{ID: "nextcloud", Title: "Nextcloud", URL: u}}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	hs, err := house.New(hcfg, s.st, s.cfg.MasterKey, log)
	if err != nil {
		t.Fatal(err)
	}
	portalConns, portalOnboarding, s.svc.Disconnect = conns{hs}, onboarding{s.st}, hs.DisconnectAll
	s.buildEdge(t)
	workspace := mcp.New(mcp.Server{SearXNG: &url.URL{}, SlotOf: s.cfg.SlotOf, Users: s.st, Usage: s.st, Log: log,
		Nextcloud: ncTools(hs, hcfg[0], s.svc)})

	type seen struct{ user, login, pw string }
	var mu sync.Mutex
	var pws []seen
	probe := http.NewServeMux()
	probe.HandleFunc("/list", func(w http.ResponseWriter, r *http.Request) {
		req := httptest.NewRequest("POST", "http://llm-gw:4000/mcp/workspace", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nextcloud_list","arguments":{}}}`))
		req.RemoteAddr = s.cfg.SlotAddr(1, config.HostUser).String() + ":40000"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		workspace.ServeHTTP(w, req)
	})
	probe.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if du, dl, dp, err := hs.Creds(r.Context(), alice.uid, "nextcloud"); err == nil {
			if len(pws) == 0 || pws[len(pws)-1].pw != dp {
				pws = append(pws, seen{du, dl, dp})
			}
		}
		for i, p := range pws {
			req, _ := http.NewRequestWithContext(r.Context(), "PROPFIND", u.String()+"/remote.php/dav/files/"+url.PathEscape(p.user)+"/", nil)
			req.SetBasicAuth(p.login, p.pw)
			req.Header.Set("Depth", "0")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				fmt.Fprintf(w, "app password %d: %v\n", i+1, err)
				continue
			}
			resp.Body.Close()
			fmt.Fprintf(w, "app password %d: %d\n", i+1, resp.StatusCode)
		}
	})
	go http.ListenAndServe("127.0.0.1:47823", probe)

	s.svc.PurgeData = func(context.Context, string, int) error { return nil }
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Authentik-Uid", alice.uid)
		r.Header.Set("X-Authentik-Username", user)
		r.Header.Set("X-Authentik-Name", "Testkonto")
		r.Header.Set("X-Authentik-Groups", alice.groups)
		r.Host = "apps.example.org"
		if r.Header.Get("Origin") != "" {
			r.Header.Set("Origin", "https://apps.example.org")
		}
		s.edge.ServeHTTP(w, r)
	})
	t.Log("portal on http://127.0.0.1:47822, probe on http://127.0.0.1:47823")
	t.Fatal(http.ListenAndServe("127.0.0.1:47822", h))
}
