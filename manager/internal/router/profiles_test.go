// SPDX-License-Identifier: MIT

package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
)

func TestStagedAssistantRefusesRuntimeRoutes(t *testing.T) {
	calls := 0
	rt, _ := newRouter("http://runtime.invalid", true)
	rt.proxy.Transport = assistantTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected runtime call")
	})
	rt.RuntimeAccess = func(_ context.Context, uid string) error {
		if uid != uidAlice {
			t.Fatalf("wrong owner %s", uid)
		}
		return assistants.ErrIsolationRequired
	}
	for _, path := range []string{"/api/session", "/api/session/ses_child1234/prompt", "/api/session/ses_root1234/shell", "/api/config", "/api/pty"} {
		r := authed("POST", "https://alice.apps.example.org"+path)
		r.Header.Set("X-Profile-Id", "helper")
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("runtime calls: %d", calls)
	}
}

type assistantTransport func(*http.Request) (*http.Response, error)

func (p assistantTransport) RoundTrip(r *http.Request) (*http.Response, error) { return p(r) }

func TestAssistantDeepLinkRequiresLoginAndConsent(t *testing.T) {
	rt, _ := newRouter("", false)
	rt.Consented = func(context.Context, string) (bool, error) { return false, nil }
	for _, target := range []string{"/new?profile=it-hilfe", "/support/ses_profile_forged"} {
		r := authed("GET", "https://apps.example.org"+target)
		r.Header.Set("Accept", "text/html")
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		if w.Code != http.StatusFound || w.Header().Get("Location") != "https://apps.example.org/consent?next="+url.QueryEscape(target) {
			t.Fatalf("consent/deep link: %d %s", w.Code, w.Header().Get("Location"))
		}
		r.Header.Del("X-Authentik-Uid")
		w = httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("anonymous profile access: %d", w.Code)
		}
	}
	r := authed("POST", "https://apps.example.org/new")
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("created without consent: %d", w.Code)
	}
}
