// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func TestConsent(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	at := time.UnixMilli(time.Now().UnixMilli())
	// before the account exists (the router stores consent first)
	if err := s.AcceptConsent(ctx, uid(1), "v1", at); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasConsent(ctx, uid(1), "v1"); !ok {
		t.Fatal("v1 accepted")
	}
	if ok, _ := s.HasConsent(ctx, uid(1), "v2"); ok {
		t.Fatal("a new version needs a new consent")
	}
	if ok, err := s.HasConsent(ctx, uid(2), "v1"); ok || err != nil {
		t.Fatalf("never accepted: %v %v", ok, err)
	}
	if err := s.AcceptConsent(ctx, uid(1), "v2", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if v, got, _ := s.Consent(ctx, uid(1)); v != "v2" || !got.Equal(at.Add(time.Hour)) {
		t.Fatalf("upsert: %s %s", v, got)
	}
	if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "anna"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteUser(ctx, uid(1), "geloescht-x"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasConsent(ctx, uid(1), "v2"); ok {
		t.Fatal("account deletion removes the consent")
	}
}
