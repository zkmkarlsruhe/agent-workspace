// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Consent returns the accepted notice version and when; ErrNotFound if never.
func (s *Store) Consent(ctx context.Context, uid string) (version string, at time.Time, err error) {
	var ms int64
	err = s.db.QueryRowContext(ctx, `SELECT version, accepted_at FROM consents WHERE uid = ?`, uid).Scan(&version, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	return version, time.UnixMilli(ms), err
}

// HasConsent reports whether uid accepted exactly this version of the notice.
func (s *Store) HasConsent(ctx context.Context, uid, version string) (bool, error) {
	v, _, err := s.Consent(ctx, uid)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil && v == version, err
}

// AcceptConsent records that uid accepted the notice in this version.
func (s *Store) AcceptConsent(ctx context.Context, uid, version string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO consents (uid, version, accepted_at) VALUES (?, ?, ?)
		ON CONFLICT (uid) DO UPDATE SET version = excluded.version, accepted_at = excluded.accepted_at`,
		uid, version, at.UnixMilli())
	return err
}

// SpentSince sums the person's metered cost in euro from since on (daily
// cap), including what a deleted account of the same uid spent (spend_ledger).
func (s *Store) SpentSince(ctx context.Context, uid string, since time.Time) (float64, error) {
	var eur float64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT SUM(cost_eur) FROM usage_events WHERE uid = ? AND ts_start >= ?), 0)
		+ COALESCE((SELECT SUM(cost_eur) FROM spend_ledger WHERE uid = ? AND ts >= ?), 0)`,
		uid, since.UnixMilli(), uid, since.UnixMilli()).Scan(&eur)
	return eur, err
}
