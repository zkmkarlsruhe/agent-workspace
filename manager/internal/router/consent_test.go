// SPDX-License-Identifier: MIT

package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type countingUsers struct {
	users
	n int
}

func (c *countingUsers) EnsureUser(ctx context.Context, id identity.Identity) (store.User, error) {
	c.n++
	return c.users.EnsureUser(ctx, id)
}

func TestConsentGate(t *testing.T) {
	var hit int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit++; io.WriteString(w, "oc") }))
	defer backend.Close()
	rt, _ := newRouter(backend.URL, true)
	cu := &countingUsers{users: users{uidAlice: "alice"}}
	rt.Users = cu
	ok := false
	rt.Consented = func(_ context.Context, uid string) (bool, error) { return ok && uid == uidAlice, nil }
	do := func(method, u string, h ...string) *httptest.ResponseRecorder {
		r := authed(method, u)
		for i := 0; i+1 < len(h); i += 2 {
			r.Header.Set(h[i], h[i+1])
		}
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		return w
	}
	gate := func(u string) string { return "https://apps.example.org/consent?next=" + url.QueryEscape(u) }

	// portal: page loads go to the notice and remember where they wanted to go
	for _, p := range []string{"/", "/projects/bericht?x=1", "/environment", "/help", "/usage", "/onboarding", "/tools"} {
		w := do("GET", "https://apps.example.org"+p, "Accept", "text/html")
		if w.Code != 302 || w.Header().Get("Location") != gate(p) {
			t.Errorf("portal %s: %d %q", p, w.Code, w.Header().Get("Location"))
		}
	}
	// everything else on the portal is refused, never served
	for _, c := range [][2]string{{"POST", "/projects/start"}, {"POST", "/environment/start"}, {"GET", "/projects/x/zip"}} {
		if w := do(c[0], "https://apps.example.org"+c[1]); w.Code != 403 || w.Body.String() == "portal:" {
			t.Errorf("portal %s %s: %d", c[0], c[1], w.Code)
		}
	}
	// allowed list: served without consent and without creating the account
	for _, p := range []string{"/consent", "/consent?next=%2F", "/consent/declined", "/privacy", "/how-it-works", "/deleted",
		"/health", "/static/css/app.css", "/favicon.ico"} {
		if w := do("GET", "https://apps.example.org"+p, "Accept", "text/html"); w.Code != 200 || w.Body.String() != "portal:" {
			t.Errorf("allowed %s: %d %q", p, w.Code, w.Body.String())
		}
	}
	if w := do("POST", "https://apps.example.org/consent"); w.Code != 200 {
		t.Errorf("consent POST: %d", w.Code)
	}
	// the language switch in the header of the notice
	if w := do("POST", "https://apps.example.org/lang"); w.Code != 200 || w.Body.String() != "portal:" {
		t.Errorf("language POST: %d", w.Code)
	}
	if cu.n != 0 {
		t.Errorf("account created before consent: %d lookups", cu.n)
	}

	// person host: navigation to the portal notice with the full address, API/SSE/WS 403
	w := do("GET", "https://alice.apps.example.org/server/abc/session/ses_1?q=1", "Sec-Fetch-Mode", "navigate")
	if w.Code != 302 || w.Header().Get("Location") != gate("https://alice.apps.example.org/server/abc/session/ses_1?q=1") {
		t.Errorf("person host nav: %d %q", w.Code, w.Header().Get("Location"))
	}
	for _, c := range []struct{ m, p string }{{"GET", "/api/event"}, {"POST", "/api/session"}, {"GET", "/api/info"}, {"GET", "/__workspace/brand.css"}, {"GET", brandTitlePath()}, {"GET", "/sw.js"}} {
		if w := do(c.m, "https://alice.apps.example.org"+c.p, "Origin", "https://alice.apps.example.org"); w.Code != 403 {
			t.Errorf("person host %s %s: %d", c.m, c.p, w.Code)
		}
	}
	if w := do("GET", "https://alice.apps.example.org/api/pty/1/connect", "Upgrade", "websocket", "Origin", "https://alice.apps.example.org",
		"Accept", "text/html"); w.Code != 403 {
		t.Errorf("person host WS: %d", w.Code)
	}
	// after account deletion (no consent, no account) the browser cleanup still runs
	if w := do("GET", "https://alice.apps.example.org/__workspace/clear", "Sec-Fetch-Site", "same-site", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document"); w.Code != 303 ||
		w.Header().Get("Clear-Site-Data") != `"cache", "storage"` || w.Header().Get("Location") != "https://apps.example.org/deleted" {
		t.Errorf("clear without consent: %d %v", w.Code, w.Header())
	}
	if w := do("POST", "https://alice.apps.example.org/__workspace/clear", "Origin", "https://alice.apps.example.org"); w.Code != 403 {
		t.Errorf("clear POST: %d", w.Code)
	}
	if hit != 0 || cu.n != 0 {
		t.Fatalf("OpenCode reached (%d) or account created (%d) without consent", hit, cu.n)
	}

	ok = true
	if w := do("GET", "https://apps.example.org/"); w.Body.String() != "portal:alice" {
		t.Errorf("portal after consent: %q", w.Body.String())
	}
	if w := do("GET", "https://alice.apps.example.org/api/info"); w.Code != 200 || hit != 1 {
		t.Errorf("person host after consent: %d", w.Code)
	}
}

func TestOutpostRequiresConsent(t *testing.T) {
	rt, _ := newRouter("http://unused", false)
	cu := &countingUsers{users: users{uidAlice: "alice"}}
	rt.Users = cu
	rt.Consented = func(context.Context, string) (bool, error) { return false, nil }
	for _, host := range []string{"apps.example.org", "alice.apps.example.org"} {
		for _, method := range []string{"GET", "POST"} {
			r := authed(method, "https://"+host+"/outpost.goauthentik.io/sign_out")
			r.Header.Set("Sec-Fetch-Mode", "navigate")
			w := httptest.NewRecorder()
			rt.ServeHTTP(w, r)
			want := http.StatusForbidden
			if method == "GET" {
				want = http.StatusFound
			}
			if w.Code != want || cu.n != 0 {
				t.Fatalf("%s %s: status=%d accounts=%d", method, host, w.Code, cu.n)
			}
		}
	}
}

func TestClearRequiresNavigation(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		rt, _ := newRouter("http://unused", false)
		cu := &countingUsers{users: users{uidAlice: "alice"}}
		if deleted {
			cu.users = users{}
		}
		rt.Users = cu
		rt.Consented = func(context.Context, string) (bool, error) { return !deleted, nil }
		for _, tt := range []struct {
			host, site, mode, dest string
			want                   int
		}{
			{"alice", "same-origin", "navigate", "document", 303},
			{"alice", "same-site", "navigate", "document", 303},
			{"alice", "none", "navigate", "document", 303},
			{"other", "same-site", "navigate", "document", 303},
			{"alice", "cross-site", "navigate", "document", 403},
			{"alice", "same-site", "no-cors", "document", 403},
			{"alice", "same-site", "navigate", "image", 403},
			{"other", "cross-site", "no-cors", "image", 403},
			{"alice", "", "navigate", "document", 403},
			{"alice", "same-site", "", "document", 403},
			{"alice", "same-site", "navigate", "", 403},
			{"alice", "", "", "", 403},
		} {
			r := authed("GET", "https://"+tt.host+".apps.example.org/__workspace/clear")
			r.Header.Set("Sec-Fetch-Site", tt.site)
			r.Header.Set("Sec-Fetch-Mode", tt.mode)
			r.Header.Set("Sec-Fetch-Dest", tt.dest)
			r.Header.Set("Referer", "https://apps.example.org/environment")
			w := httptest.NewRecorder()
			rt.ServeHTTP(w, r)
			if w.Code != tt.want || (w.Header().Get("Clear-Site-Data") != "") != (tt.want == 303) {
				t.Fatalf("deleted=%v %+v: %d %v", deleted, tt, w.Code, w.Header())
			}
			if tt.want == 303 && (w.Header().Get("Clear-Site-Data") != `"cache", "storage"` || w.Header().Get("Location") != "https://apps.example.org/deleted") {
				t.Fatalf("clear redirect: %v", w.Header())
			}
			if cu.n != 0 {
				t.Fatal("cleanup looked up or recreated the account")
			}
		}
	}
}
