// SPDX-License-Identifier: MIT

//go:build brandharness

package router

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

// go test -tags brandharness -run TestHarness -timeout 0 ./internal/router
// Browse http://alice.localhost:47812 ; OpenCode at $OC (default 127.0.0.1:47811).
func TestHarness(t *testing.T) {
	oc := os.Getenv("OC")
	if oc == "" {
		oc = "http://127.0.0.1:47811"
	}
	rt := New(Router{
		Domain:    "localhost",
		Checker:   identity.Checker{Trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, RequiredGroup: "opencode-users"},
		Users:     users{uidAlice: "alice"},
		LC:        &fakeLC{target: oc, running: true},
		Portal:    http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("portal")) }),
		Log:       slog.Default(),
		Consented: func(context.Context, string) (bool, error) { return true, nil },
	})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Authentik-Uid", uidAlice)
		r.Header.Set("X-Authentik-Username", "alice")
		r.Header.Set("X-Authentik-Groups", "opencode-users")
		if o := r.Header.Get("Origin"); strings.HasPrefix(o, "http://") {
			r.Header.Set("Origin", "https://"+strings.Split(strings.TrimPrefix(o, "http://"), ":")[0])
		}
		r.Host = strings.Split(r.Host, ":")[0]
		rt.ServeHTTP(w, r)
	})
	t.Fatal(http.ListenAndServe("127.0.0.1:47812", h))
}
