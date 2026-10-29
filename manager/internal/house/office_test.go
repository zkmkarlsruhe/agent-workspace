// SPDX-License-Identifier: MIT

package house

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDisconnectBlocksCredentialsBeforeBrowserCleanupAndFailedRevoke(t *testing.T) {
	s, nc, st := setup(t)
	if _, state := connect(t, s, nc, "alice"); state != FlowDone {
		t.Fatal(state)
	}
	called := 0
	s.Invalidate = func(ctx context.Context, uid, id string) error {
		called++
		if _, _, _, err := s.Creds(ctx, uid, id); !errors.Is(err, ErrNotConnected) {
			t.Fatal("credentials still usable during cleanup")
		}
		return nil
	}
	nc.mu.Lock()
	nc.noRevoke = true
	nc.mu.Unlock()
	if err := s.Disconnect(context.Background(), uid1, "nextcloud"); err == nil {
		t.Fatal("failed revoke hidden")
	}
	// A late successful old request may touch last_used, but may not clear the durable block.
	if err := st.ConnectionUsed(context.Background(), uid1, "nextcloud", time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Creds(context.Background(), uid1, "nextcloud"); !errors.Is(err, ErrNotConnected) {
		t.Fatal("revoking login revived")
	}
	if called != 1 {
		t.Fatal("browser cleanup not called")
	}
	nc.mu.Lock()
	nc.noRevoke = false
	nc.mu.Unlock()
	if err := s.Disconnect(context.Background(), uid1, "nextcloud"); err != nil {
		t.Fatal(err)
	}
}
