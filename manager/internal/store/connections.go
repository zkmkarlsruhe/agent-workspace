// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Connection is a person's stored login to a house service (docs/adr/README.md
// §4.1). SecretEnc is sealed by internal/house; the store never sees plain text.
type Connection struct {
	UID, Service, Login string
	SecretEnc           []byte
	ConnectedAt         time.Time
	LastUsed            time.Time
	Expired             bool // the service answered 401 on the last use
}

const connCols = `uid, service, login, secret_enc, connected_at, last_used, expired`

func scanConn(r scanner) (Connection, error) {
	var c Connection
	var at, used int64
	err := r.Scan(&c.UID, &c.Service, &c.Login, &c.SecretEnc, &at, &used, &c.Expired)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.ConnectedAt, c.LastUsed = time.UnixMilli(at), time.UnixMilli(used)
	return c, err
}

// PutConnection stores or replaces the connection of (uid, service).
func (s *Store) PutConnection(ctx context.Context, c Connection) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO connections (`+connCols+`) VALUES (?, ?, ?, ?, ?, 0, 0)`,
		c.UID, c.Service, c.Login, c.SecretEnc, c.ConnectedAt.UnixMilli())
	return err
}

func (s *Store) Connection(ctx context.Context, uid, service string) (Connection, error) {
	return scanConn(s.db.QueryRowContext(ctx, `SELECT `+connCols+` FROM connections WHERE uid = ? AND service = ?`, uid, service))
}

func (s *Store) DeleteConnection(ctx context.Context, uid, service string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connections WHERE uid = ? AND service = ?`, uid, service)
	return err
}

// ConnectionUsed records a use; expired marks a 401 from the service.
func (s *Store) ConnectionUsed(ctx context.Context, uid, service string, at time.Time, expired bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE connections SET last_used = ?, expired = ? WHERE uid = ? AND service = ?`,
		at.UnixMilli(), expired, uid, service)
	return err
}

// HeldServices lists the house services that hold a login of uid: a stored
// connection or a queued revocation. Account deletion must clear these
// whatever the current configuration says.
func (s *Store) HeldServices(ctx context.Context, uid string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT service FROM connections WHERE uid = ?
		UNION SELECT service FROM revocations WHERE uid = ? ORDER BY 1`, uid, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sv string
		if err := rows.Scan(&sv); err != nil {
			return nil, err
		}
		out = append(out, sv)
	}
	return out, rows.Err()
}

// Revocation is an app password still to revoke; SecretEnc as in Connection.
type Revocation struct {
	ID           int64
	UID, Service string
	SecretEnc    []byte
}

func (s *Store) AddRevocation(ctx context.Context, uid, service string, secretEnc []byte, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO revocations (uid, service, secret_enc, added_at) VALUES (?, ?, ?, ?)`,
		uid, service, secretEnc, at.UnixMilli())
	return err
}

// Revocations lists the pending revocations of uid, or of everyone if uid is "".
func (s *Store) Revocations(ctx context.Context, uid string) ([]Revocation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, uid, service, secret_enc FROM revocations WHERE ? = '' OR uid = ? ORDER BY id`, uid, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revocation
	for rows.Next() {
		var r Revocation
		if err := rows.Scan(&r.ID, &r.UID, &r.Service, &r.SecretEnc); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteRevocation(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM revocations WHERE id = ?`, id)
	return err
}

// OnboardingSeen reports whether the person was already shown the Onboarding.
func (s *Store) OnboardingSeen(ctx context.Context, uid string) (bool, error) {
	var x int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM onboarding WHERE uid = ?`, uid).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) MarkOnboardingSeen(ctx context.Context, uid string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO onboarding (uid, seen_at) VALUES (?, ?)`, uid, at.UnixMilli())
	return err
}
