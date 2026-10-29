// SPDX-License-Identifier: MIT

package house

import (
	"context"
	"errors"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// A grant that completes after Disconnect (or after a newer Connect) is
// revoked, never stored.
func TestGrantOfEndedFlowRevoked(t *testing.T) {
	s, nc, st := setup(t)
	ctx := context.Background()
	if _, err := s.Connect(ctx, uid1, "alice", "nextcloud"); err != nil {
		t.Fatal(err)
	}
	ended := &Flow{} // the poll's flow, no longer s.flows[key] after Disconnect
	if err := s.Disconnect(ctx, uid1, "nextcloud"); err != nil {
		t.Fatal(err)
	}
	pw := nc.grant("alice")
	nc.mu.Lock()
	g := *nc.pending
	nc.valid[g.AppPassword], nc.pending = g.LoginName, nil
	nc.mu.Unlock()
	if state := s.finish(ctx, ended, s.Services[0], uid1, "alice", g); state != FlowFailed {
		t.Fatalf("state %v", state)
	}
	if _, err := st.Connection(ctx, uid1, "nextcloud"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("connected after Disconnect")
	}
	if nc.isValid(pw) {
		t.Fatal("grant of the ended flow left valid")
	}
}

// A revocation Nextcloud does not confirm is kept and retried; account
// deletion fails while one is pending.
func TestFailedRevocationRetried(t *testing.T) {
	s, nc, st := setup(t)
	ctx := context.Background()
	old, _ := connect(t, s, nc, "alice")
	nc.mu.Lock()
	nc.noRevoke = true
	nc.mu.Unlock()
	if _, state := connect(t, s, nc, "alice"); state != FlowDone {
		t.Fatalf("reconnect: %v", state)
	}
	if q, _ := st.Revocations(ctx, uid1); len(q) != 1 || !nc.isValid(old) {
		t.Fatalf("queue %d, old valid %v", len(q), nc.isValid(old))
	}
	if err := s.RetryRevocations(ctx, ""); err == nil {
		t.Fatal("retry reported success while Nextcloud refuses")
	}
	if err := s.DisconnectAll(ctx, uid1); err == nil {
		t.Fatal("account deletion went on with a pending revocation")
	}
	nc.mu.Lock()
	nc.noRevoke = false
	nc.mu.Unlock()
	if err := s.RetryRevocations(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if q, _ := st.Revocations(ctx, ""); len(q) != 0 || nc.isValid(old) {
		t.Fatalf("queue %d, old valid %v", len(q), nc.isValid(old))
	}
	if err := s.DisconnectAll(ctx, uid1); err != nil {
		t.Fatal(err)
	}
}
