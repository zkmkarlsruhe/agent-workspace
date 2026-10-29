// SPDX-License-Identifier: MIT

//go:build portalharness

package main

import (
	"net/http"
	"testing"
)

// go test -tags portalharness -run TestHarnessSettings -timeout 0 ./cmd/manager
// Portal on http://127.0.0.1:47824 as alice with the personal settings page; directory
// facts from a stub, AGENTS.md in a temp file.
func TestHarnessSettings(t *testing.T) {
	s := newStack(t, "127.88")
	seed(t, s)
	agents := withSettings(t, s)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Authentik-Uid", alice.uid)
		r.Header.Set("X-Authentik-Username", alice.username)
		r.Header.Set("X-Authentik-Name", "the operator")
		r.Header.Set("X-Authentik-Email", "alice.example@example.org")
		r.Header.Set("X-Authentik-Groups", alice.groups)
		r.Host = "apps.example.org"
		if r.Header.Get("Origin") != "" {
			r.Header.Set("Origin", "https://apps.example.org")
		}
		s.edge.ServeHTTP(w, r)
	})
	t.Log("portal on http://127.0.0.1:47824, AGENTS.md at " + agents)
	t.Fatal(http.ListenAndServe("127.0.0.1:47824", h))
}
