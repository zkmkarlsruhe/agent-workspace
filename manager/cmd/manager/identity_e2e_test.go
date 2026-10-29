// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestIdentityGuardE2E(t *testing.T) {
	s := newStack(t, "127.93")
	s.cfg.AdminGroup = "operations-admins"
	s.buildEdge(t)
	ctx := context.Background()
	issuer := s.cfg.IdentityIssuer

	// The environment page binds without starting a container.
	want(t, s.do(t, call{url: "/environment", as: &alice}), http.StatusOK)
	original, err := s.st.UserByUID(ctx, alice.uid)
	if err != nil {
		t.Fatal(err)
	}
	want(t, s.do(t, call{url: "/environment", as: &alice}), http.StatusOK)
	if err := s.st.CreateProject(ctx, store.Project{ID: "private", UID: alice.uid, Folder: "private-project", Description: "private-description", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	incoming := person{strings.Repeat("b", 64), alice.username, alice.groups}
	paths := []string{"/", "/projects/private-project", "/usage", "/consent", "/admin", "https://alice.apps.example.org/api/info"}
	for _, path := range paths {
		w := s.do(t, call{url: path, as: &incoming, noConsent: true})
		want(t, w, http.StatusForbidden, "Dieses Konto ist gesperrt")
		for _, secret := range []string{"private-project", "private-description", alice.uid, incoming.uid} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("blocked page leaks %q", secret)
			}
		}
	}
	want(t, s.do(t, call{method: "POST", url: "/environment/start", as: &incoming, noConsent: true}), http.StatusForbidden)
	want(t, s.do(t, call{url: "/", as: &incoming, noConsent: true, hdr: []string{"Accept-Language", "en"}}), http.StatusForbidden,
		"This account is blocked. Contact the administrator.")
	want(t, s.do(t, call{url: "/static/css/app.css", as: &incoming, noConsent: true}), http.StatusOK)
	wantLoc(t, s.do(t, call{method: "POST", url: "/lang", as: &incoming, noConsent: true, form: url.Values{"lang": {"en"}}}), "/")
	if language, err := s.st.Lang(ctx, alice.uid); err != nil || language != "" {
		t.Fatalf("blocked request changed account preferences: %q %v", language, err)
	}
	if u, err := s.st.UserByUID(ctx, alice.uid); err != nil || u.Generation != 0 || len(s.rt.Ctrs) != 0 {
		t.Fatalf("blocked login started an environment: %+v %v", u, err)
	}

	blocks, err := s.st.IdentityBlocks(ctx)
	if err != nil || len(blocks) != 1 || blocks[0].PendingHash != identity.SubjectHash(issuer, incoming.uid) {
		t.Fatalf("pending subject: %+v %v", blocks, err)
	}
	release := "/admin/identity/" + alice.uid + "/release"
	form := url.Values{"subject_hash": {blocks[0].PendingHash}}
	operator := person{strings.Repeat("d", 64), "operator", "opencode-users|operations-admins"}
	outsider := person{strings.Repeat("e", 64), "outsider", "opencode-users"}
	want(t, s.do(t, call{method: "POST", url: release, form: form, as: &outsider}), http.StatusNotFound)
	want(t, s.do(t, call{url: "/admin", as: &operator}), http.StatusOK, "Gesperrte Konten", blocks[0].PendingHash)
	want(t, s.do(t, call{method: "POST", url: release, form: form, as: &operator,
		hdr: []string{"Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"}}), http.StatusForbidden)
	want(t, s.do(t, call{method: "POST", url: release, form: url.Values{"subject_hash": {"stale"}}, as: &operator}), http.StatusConflict)
	wantLoc(t, s.do(t, call{method: "POST", url: release, form: form, as: &operator}), "/admin")
	want(t, s.do(t, call{method: "POST", url: release, form: form, as: &operator}), http.StatusConflict)

	// Consent and all data use the old account key after release.
	want(t, s.do(t, call{url: "/", as: &incoming, noConsent: true}), http.StatusOK, "private-project")
	s.waitRunning(t)
	want(t, s.do(t, call{url: "https://alice.apps.example.org/api/info", as: &incoming, noConsent: true}), http.StatusOK)
	if got, err := s.st.UserByUID(ctx, alice.uid); err != nil || got.Slot != original.Slot || got.Slug != original.Slug {
		t.Fatalf("rebind moved data: %+v %v", got, err)
	}
	if _, err := s.st.UserByUID(ctx, incoming.uid); err != store.ErrNotFound {
		t.Fatalf("rebind created another account: %v", err)
	}
	// Provider metadata changes leave the instance issuer and binding unchanged.
	want(t, s.do(t, call{url: "https://alice.apps.example.org/api/info", as: &incoming, noConsent: true,
		hdr: []string{"X-Authentik-Meta-Jwks", "https://other.example/jwks/"}}), http.StatusOK)
}

func TestLegacyIdentityBindE2E(t *testing.T) {
	s := newStack(t, "127.94")
	ctx := context.Background()
	legacy, err := s.st.EnsureUser(ctx, identity.Identity{UID: alice.uid, Username: alice.username})
	if err != nil {
		t.Fatal(err)
	}
	want(t, s.do(t, call{url: "/environment", as: &alice}), http.StatusOK)
	want(t, s.do(t, call{url: "/environment", as: &alice}), http.StatusOK)
	incoming := person{strings.Repeat("b", 64), alice.username, alice.groups}
	want(t, s.do(t, call{url: "/", as: &incoming}), http.StatusForbidden)
	if got, err := s.st.UserByUID(ctx, alice.uid); err != nil || got != legacy || len(s.rt.Ctrs) != 0 {
		t.Fatalf("legacy identity changed or started: %+v %v", got, err)
	}
}

func TestIdentityIssuerRefusesStartup(t *testing.T) {
	// run selects a new instance before rejecting the issuer; restore the test fixture and timezone.
	t.Cleanup(instance.Select(instance.Current()))
	path := filepath.Join(t.TempDir(), "manager.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.EnsureUser(context.Background(), identity.Identity{UID: alice.uid, Username: alice.username, Issuer: "authentik", Subject: alice.uid})
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OC_INSTANCE_FILE", "../../../instance/example/instance.yaml")
	t.Setenv("OC_DB_PATH", path)
	t.Setenv("OC_IDENTITY_ISSUER", "different-idp")
	t.Setenv("OC_TRUSTED_PROXY_CIDRS", "127.0.0.0/8")
	t.Setenv("OC_RUNTIME_IMAGE", "test-image")
	t.Setenv("OC_MASTER_KEY", strings.Repeat("k", 32))
	t.Setenv("LITELLM_API_KEY", "test-key")
	// No Docker client, runtime or listener may be initialized before this fails.
	if err := run(slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "OC_IDENTITY_ISSUER") || !strings.Contains(err.Error(), "1 account(s)") {
		t.Fatalf("issuer startup gate: %v", err)
	}
}
