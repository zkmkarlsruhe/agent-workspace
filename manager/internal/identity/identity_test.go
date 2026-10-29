// SPDX-License-Identifier: MIT

package identity

import (
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

const uid = "a7889ac42c1d44753644ec24acde57e8650e05b30d4b86f39dc0c96a98d25356"

func TestFromRequest(t *testing.T) {
	c := Checker{Trusted: []netip.Prefix{netip.MustParsePrefix("172.20.0.0/16")}, RequiredGroup: "opencode-users"}
	cases := []struct {
		name, remote, uid, user, groups string
		want                            error
	}{
		{"ok", "172.20.0.5:1234", uid, "alice", "a|opencode-users", nil},
		{"untrusted source", "10.90.1.10:1234", uid, "alice", "opencode-users", ErrUntrusted},
		{"uppercase uid", "172.20.0.5:1", strings.ToUpper(uid), "x", "opencode-users", ErrInvalid},
		{"short uid", "172.20.0.5:1", uid[:63], "x", "opencode-users", ErrInvalid},
		{"no username", "172.20.0.5:1", uid, " ", "opencode-users", ErrInvalid},
		{"no group", "172.20.0.5:1", uid, "x", "other", ErrNoGroup},
		{"ipv4-mapped", "[::ffff:172.20.0.5]:1", uid, "x", "opencode-users", nil},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "https://apps.example.org/", nil)
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Authentik-Uid", tc.uid)
		r.Header.Set("X-Authentik-Username", tc.user)
		r.Header.Set("X-Authentik-Groups", tc.groups)
		_, err := c.FromRequest(r)
		if err != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, err, tc.want)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"alice":                    "alice",
		"Jürgen.Groß":              "juergen-gross",
		"--a__b--":                 "a-b",
		"René Müller-Lüdenscheidt": "rene-mueller-luedenscheidt",
		"42bob":                    "u-42bob",
		"":                         "",
		"a-very-long-username-that-exceeds-the-limit": "a-very-long-username-that-exceed",
	}
	for in, want := range cases {
		got := Slugify(in)
		if got != want {
			t.Errorf("Slugify(%q) = %q want %q", in, got, want)
		}
		if strings.Contains(got, "--") || len(got) > MaxSlugLen {
			t.Errorf("Slugify(%q) = %q violates rules", in, got)
		}
	}
}

func TestCandidate(t *testing.T) {
	if Candidate("admin", 1) != "" || Candidate("a", 1) != "" {
		t.Fatal("reserved/short base must be skipped")
	}
	if got := Candidate("admin", 2); got != "admin-2" {
		t.Fatal(got)
	}
	if got := Candidate("", 2); got != "user-2" {
		t.Fatal(got)
	}
	long := strings.Repeat("a", 31) + "-b"
	if got := Candidate(long[:32], 3); len(got) > MaxSlugLen || strings.Contains(got, "--") {
		t.Fatal(got)
	}
}

func TestSubjectHeaders(t *testing.T) {
	c := Checker{Trusted: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}
	r := httptest.NewRequest("GET", "https://workspace.example/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("X-Authentik-Uid", uid)
	r.Header.Set("X-Authentik-Username", "alice")
	id, err := c.FromRequest(r)
	if err != nil || id.Subject != uid || id.Issuer != "authentik" {
		t.Fatalf("default issuer and subject: %+v %v", id, err)
	}
	c.Issuer, c.SubjectHeader = "https://id.example/", "X-Identity-Subject"
	// Per-request metadata cannot change the configured namespace.
	r.Header.Set("X-Authentik-Meta-Jwks", "https://other.example/new-provider/jwks/")
	r.Header.Set("X-Identity-Issuer", "untrusted-issuer")
	r.Header.Set(c.SubjectHeader, "stable-person-42")
	id, err = c.FromRequest(r)
	if err != nil || id.Subject != "stable-person-42" || id.Issuer != "https://id.example/" || id.UID != uid {
		t.Fatalf("custom subject headers: %+v %v", id, err)
	}
	r.Header.Add(c.SubjectHeader, "another-person")
	if _, err := c.FromRequest(r); err != ErrInvalid {
		t.Fatalf("duplicate subject header accepted: %v", err)
	}
	for _, bad := range []string{"", " ", " leading", "trailing ", "a\x00b", strings.Repeat("x", 2049)} {
		r.Header.Set(c.SubjectHeader, bad)
		if _, err := c.FromRequest(r); err != ErrInvalid {
			t.Errorf("malformed subject accepted: %v", err)
		}
	}
	if SubjectHash("ab", "c") == SubjectHash("a", "bc") || SubjectHash("issuer1", uid) == SubjectHash("issuer2", uid) {
		t.Fatal("subject hash does not separate issuer and subject")
	}
}

func TestMappedIdentityAndDerivedUID(t *testing.T) {
	c := Checker{Trusted: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		Issuer: "https://id.example/", SubjectHeader: "X-Auth-Request-User",
		Headers:         Headers{UID: "X-Internal-Uid", Username: "X-Auth-Request-Preferred-Username", Email: "X-Auth-Request-Email", Name: "X-Auth-Request-Name", Groups: "X-Auth-Request-Groups"},
		GroupsSeparator: ",", RequiredGroup: "workspace-users"}
	r := httptest.NewRequest("GET", "https://workspace.example/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set(c.SubjectHeader, "8f7b4450-f52e-4faa-bf16-743c46a2ec85")
	r.Header.Set(c.Headers.Username, " alice ")
	r.Header.Set(c.Headers.Email, "alice@example.org")
	r.Header.Set(c.Headers.Name, "Alice Example")
	r.Header.Set(c.Headers.Groups, " other, workspace-users,workspace-admins,, ")
	r.Header.Set("X-Authentik-Username", "forged")
	for _, bad := range []string{"", "invalid", strings.ToUpper(uid), uid[:63]} {
		r.Header.Set(c.Headers.UID, bad)
		if _, err := c.FromRequest(r); err != ErrInvalid {
			t.Fatalf("present invalid UID %q accepted: %v", bad, err)
		}
	}
	r.Header.Del(c.Headers.UID)
	id, err := c.FromRequest(r)
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(c.Issuer+"\x00"+r.Header.Get(c.SubjectHeader))))
	if err != nil || id.UID != want || !ValidUID(id.UID) || id.Username != "alice" || id.Email != "alice@example.org" || id.Name != "Alice Example" ||
		!reflect.DeepEqual(id.Groups, []string{"other", "workspace-users", "workspace-admins"}) {
		t.Fatalf("mapped identity: %+v %v", id, err)
	}
	again, err := c.FromRequest(r)
	if err != nil || again.UID != id.UID {
		t.Fatalf("unstable UID: %+v %v", again, err)
	}
	c.Issuer = "https://other.example/"
	other, err := c.FromRequest(r)
	if err != nil || other.UID == id.UID || !ValidUID(other.UID) {
		t.Fatalf("issuer separation: %+v %v", other, err)
	}
	c.Headers.UID = "-"
	r.Header.Set("X-Authentik-Uid", uid)
	r.Header.Set("X-Internal-Uid", uid)
	r.Header.Set("-", uid)
	derived, err := c.FromRequest(r)
	if err != nil || derived.UID != other.UID {
		t.Fatalf("disabled UID mapping did not derive: %+v %v", derived, err)
	}
	c.Headers.UID = "X-Internal-Uid"
	existing, err := c.FromRequest(r)
	if err != nil || existing.UID != uid {
		t.Fatalf("64-hex UID changed: %+v %v", existing, err)
	}
	r.Header.Set(c.Headers.Groups, "workspace-users-admin,other")
	if _, err := c.FromRequest(r); err != ErrNoGroup {
		t.Fatalf("partial group match: %v", err)
	}
	r.Header.Set(c.Headers.Groups, "workspace-users")
	r.Header.Add(c.SubjectHeader, "duplicate")
	if _, err := c.FromRequest(r); err != ErrInvalid {
		t.Fatalf("duplicate mapped subject: %v", err)
	}
	r.RemoteAddr = "198.51.100.1:1234"
	r.Header.Set("X-Forwarded-For", "192.0.2.1")
	if _, err := c.FromRequest(r); err != ErrUntrusted {
		t.Fatalf("forwarded peer trusted: %v", err)
	}
}
