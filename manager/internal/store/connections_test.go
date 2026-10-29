// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func TestConnectionsAndOnboarding(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "a1"})
	if _, err := s.Connection(ctx, uid(1), "nextcloud"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	at := time.UnixMilli(time.Now().UnixMilli())
	if err := s.PutConnection(ctx, Connection{UID: uid(1), Service: "nextcloud", Login: "a1", SecretEnc: []byte{1, 2}, ConnectedAt: at}); err != nil {
		t.Fatal(err)
	}
	s.ConnectionUsed(ctx, uid(1), "nextcloud", at, true)
	c, err := s.Connection(ctx, uid(1), "nextcloud")
	if err != nil || c.Login != "a1" || !c.Expired || !c.ConnectedAt.Equal(at) || len(c.SecretEnc) != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	// a reconnect replaces the row and clears expired
	s.PutConnection(ctx, Connection{UID: uid(1), Service: "nextcloud", Login: "a1", SecretEnc: []byte{3}, ConnectedAt: at})
	if c, _ = s.Connection(ctx, uid(1), "nextcloud"); c.Expired || c.SecretEnc[0] != 3 {
		t.Fatalf("%+v", c)
	}
	if seen, _ := s.OnboardingSeen(ctx, uid(1)); seen {
		t.Fatal("seen before")
	}
	s.MarkOnboardingSeen(ctx, uid(1), at)
	s.MarkOnboardingSeen(ctx, uid(1), at)
	if seen, err := s.OnboardingSeen(ctx, uid(1)); !seen || err != nil {
		t.Fatal("not seen", err)
	}
	// account deletion takes both rows with it
	if _, err := s.DeleteUser(ctx, uid(1), "geloescht-x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Connection(ctx, uid(1), "nextcloud"); !errors.Is(err, ErrNotFound) {
		t.Fatal("connection survived the account")
	}
	if seen, _ := s.OnboardingSeen(ctx, uid(1)); seen {
		t.Fatal("onboarding survived the account")
	}
}

func TestHeldServices(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "a1"})
	if l, err := s.HeldServices(ctx, uid(1)); err != nil || l != nil {
		t.Fatal(l, err)
	}
	s.PutConnection(ctx, Connection{UID: uid(1), Service: "nextcloud", Login: "a1", SecretEnc: []byte{1}, ConnectedAt: time.Now()})
	s.AddRevocation(ctx, uid(1), "nextcloud", []byte{2}, time.Now())
	s.AddRevocation(ctx, uid(1), "cloud-old", []byte{3}, time.Now())
	s.AddRevocation(ctx, uid(2), "other", []byte{4}, time.Now())
	if l, err := s.HeldServices(ctx, uid(1)); err != nil || len(l) != 2 || l[0] != "cloud-old" || l[1] != "nextcloud" {
		t.Fatal(l, err)
	}
}
