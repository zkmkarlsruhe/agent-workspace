// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func TestLang(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	// before the account: nothing stored, no row created
	if err := s.SetLang(ctx, uid(1), "en"); err != nil {
		t.Fatal(err)
	}
	if l, err := s.Lang(ctx, uid(1)); err != ErrNotFound || l != "" {
		t.Fatalf("lang without account = %q, %v", l, err)
	}
	if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.Lang(ctx, uid(1)); l != "" {
		t.Fatalf("new account has lang %q", l)
	}
	s.SetLang(ctx, uid(1), "en")
	if l, _ := s.Lang(ctx, uid(1)); l != "en" {
		t.Fatalf("lang = %q", l)
	}
	if d, err := s.LoadSettings(ctx, uid(1)); err != nil || d.Lang != "en" {
		t.Fatalf("profile lang = %q, %v", d.Lang, err)
	}
	if _, err := s.DeleteUser(ctx, uid(1), "geloescht-1"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.Lang(ctx, uid(1)); l != "" {
		t.Fatal("lang survives the account")
	}
}
