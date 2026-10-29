// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"slices"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func TestOptIns(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []struct {
		id string
		on bool
	}{{"b", true}, {"a", true}, {"a", true}, {"b", false}, {"c", false}} {
		if err := s.SetOptIn(ctx, uid(1), op.id, op.on); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.OptIns(ctx, uid(1))
	if err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, _ := s.OptIns(ctx, uid(2)); len(got) != 0 {
		t.Fatalf("other person: %v", got)
	}
	// unknown person: foreign key
	if err := s.SetOptIn(ctx, uid(3), "a", true); err == nil {
		t.Fatal("want foreign key error")
	}
}
