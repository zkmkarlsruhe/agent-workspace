// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
)

// ErrUnavailable: account deletion needs oc-provision (PurgeData), and a
// configured house service for every login still stored.
var ErrUnavailable = errors.New("account deletion unavailable")

// DeleteAccount returns the deleted person's slug (for clearing the browser data of <slug>.<domain>).
// DeleteAccount deletes everything of the person: container and network,
// the data dir with projects, trash, chats and OpenCode config/data/state
// (oc-provision purge, which also clears the quota limit), then the store
// rows, which frees slug and slot. Usage events stay, under a random
// "deleted-<hex>" instead of the uid (rows from before the English rename
// keep "geloescht-<hex>"), so cost totals remain right. The
// next visit creates a fresh account (store.EnsureUser).
//
// Order matters for retries: each step is idempotent and the rows go last,
// so a failure leaves the account in place and a second call finishes it.
// The person's lock is held throughout: a start waiting on it finds the
// account gone or freshly created.
func (s *Service) DeleteAccount(ctx context.Context, uid string) (slug string, err error) {
	if s.PurgeData == nil {
		return "", ErrUnavailable
	}
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	u, err := s.st.UserByUID(ctx, uid)
	if err != nil {
		return "", err
	}
	ctx = logx.With(ctx, logx.Fields{UID: uid, Slot: u.Slot})
	// every step logs: a failed deletion is retried, the log shows where it stopped
	step := func(name string, err error) error {
		if err != nil {
			s.log.ErrorContext(ctx, "account deletion failed", "op", name, "err", err)
		} else {
			s.log.InfoContext(ctx, "account deletion step", "op", name)
		}
		return err
	}
	s.log.InfoContext(ctx, "account deletion started", "reason", "deletion")
	if s.Disconnect != nil {
		// first: an app password left valid in Nextcloud would outlive the account
		if err := step("disconnect house services", s.Disconnect(ctx, uid)); err != nil {
			return "", fmt.Errorf("disconnect house services: %w", err)
		}
	}
	// fail closed: a login still stored here (house config removed, or a
	// service dropped from it) would go with the account row while its app
	// password stays valid in Nextcloud
	held, err := s.st.HeldServices(ctx, uid)
	if err != nil {
		return "", err
	}
	if len(held) > 0 {
		s.log.ErrorContext(ctx, "account deletion blocked: house service logins cannot be revoked, service not configured", "uid", uid, "services", held)
		return "", fmt.Errorf("%w: logins of %s cannot be revoked, service not configured", ErrUnavailable, strings.Join(held, ", "))
	}
	s.collect(ctx, uid) // the counts survive anonymised in the daily overview (store.DeleteUser)
	name := config.SlotContainer(u.Slot)
	if err := step("stop container", s.rt.Stop(ctx, name, 0)); err != nil {
		return "", fmt.Errorf("stop %s: %w", name, err)
	}
	if err := step("remove container", s.rt.Remove(ctx, name)); err != nil {
		return "", fmt.Errorf("remove %s: %w", name, err)
	}
	if err := step("remove tool containers", s.removeTools(ctx, uid, u.Slot)); err != nil {
		return "", fmt.Errorf("remove tool containers: %w", err)
	}
	s.mu.Lock()
	r := s.run[uid]
	delete(s.run, uid)
	delete(s.starts, uid)
	s.mu.Unlock()
	// a barrier: wait for execs that captured the old container; later ones find it gone
	op := s.opLock(uid)
	op.Lock()
	op.Unlock()
	// the same for HTTP calls and proxied requests: cancelled and drained before the slot is freed
	if r != nil {
		r.lease.end()
	}
	if err := step("remove network", s.rt.RemoveNetwork(ctx, config.SlotNetwork(u.Slot))); err != nil {
		return "", fmt.Errorf("network %s: %w", config.SlotNetwork(u.Slot), err)
	}
	if err := step("purge data", s.PurgeData(ctx, uid, u.Slot)); err != nil {
		return "", fmt.Errorf("purge data slot %d: %w", u.Slot, err)
	}
	d, err := s.st.DeleteUser(ctx, uid, "deleted-"+newID())
	if err := step("delete store rows", err); err != nil {
		return "", err
	}
	s.log.InfoContext(ctx, "account deleted", "uid", uid, "slot", u.Slot, "projects", d.Projects, "trash", d.Trash, "usage_events_anonymised", d.UsageEvents, "chats_anonymised", d.Chats)
	return u.Slug, nil
}
