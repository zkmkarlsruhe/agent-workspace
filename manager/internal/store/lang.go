// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
)

// Lang returns the portal language uid chose (migration 11); "" for none, ErrNotFound for no account.
func (s *Store) Lang(ctx context.Context, uid string) (string, error) {
	var l string
	err := s.db.QueryRowContext(ctx, `SELECT lang FROM users WHERE uid = ?`, uid).Scan(&l)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return l, err
}

// SetLang stores the portal language; without an account row it changes nothing.
func (s *Store) SetLang(ctx context.Context, uid, lang string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET lang = ? WHERE uid = ?`, lang, uid)
	return err
}
