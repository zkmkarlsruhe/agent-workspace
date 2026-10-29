// SPDX-License-Identifier: MIT

package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestToolTitlesAuthenticatedRuntimeOnly(t *testing.T) {
	rt, lc := newRouter("http://127.0.0.1:1", true)
	calls := 0
	rt.ToolTitles = func(ctx context.Context, u store.User, session string) (map[string]string, error) {
		calls++
		if u.UID != uidAlice || session != "ses_project0001" {
			t.Fatal("client selected identity/session", u.UID, session)
		}
		return map[string]string{"library_search": "<b>Find books</b>"}, nil
	}
	get := func(method, host, session string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, authed(method, "https://"+host+brandToolTitlesPath()+"?session="+session+"&uid=other"))
		return w
	}
	w := get("GET", "alice.apps.example.org", "ses_project0001")
	var labels map[string]string
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &labels) != nil || labels["library_search"] != "<b>Find books</b>" || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Fatal(w.Code, w.Body.String(), w.Header())
	}
	if w = get("GET", "alice.apps.example.org", "bad"); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w = get("POST", "alice.apps.example.org", "ses_project0001"); w.Code != 405 {
		t.Fatal(w.Code)
	}
	if w = get("GET", "anna.apps.example.org", "ses_project0001"); w.Code == 200 {
		t.Fatal("foreign host accepted")
	}
	rt.RuntimeAccess = func(context.Context, string) error { return errors.New("isolated runtime") }
	if w = get("GET", "alice.apps.example.org", "ses_project0001"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	rt.RuntimeAccess = nil
	lc.running = false
	if w = get("GET", "alice.apps.example.org", "ses_project0001"); w.Code != 503 {
		t.Fatal(w.Code)
	}
	lc.running = true
	rt.Consented = func(context.Context, string) (bool, error) { return false, nil }
	if w = get("GET", "alice.apps.example.org", "ses_project0001"); w.Code == 200 {
		t.Fatal("consent bypass")
	}
	if calls != 1 {
		t.Fatal("unauthorized discovery", calls)
	}
}
