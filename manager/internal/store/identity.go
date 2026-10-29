// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

var ErrIdentityBlocked = errors.New("store: identity blocked")
var ErrIdentityIssuerMismatch = errors.New("store: OC_IDENTITY_ISSUER does not match stored identities")

// IdentityBlockedError carries only hashes, including when logged as an error.
type IdentityBlockedError struct{ StoredHash, IncomingHash string }

func (e *IdentityBlockedError) Error() string { return ErrIdentityBlocked.Error() }
func (e *IdentityBlockedError) Unwrap() error { return ErrIdentityBlocked }

// CheckIdentityIssuer fails startup before any runtime or listener is started.
// An issuer change is an operator migration, never a request-driven rebind.
func (s *Store) CheckIdentityIssuer(ctx context.Context, issuer string) error {
	if !identity.ValidSubject(issuer, "subject") {
		return errors.New("store: invalid OC_IDENTITY_ISSUER")
	}
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users
		WHERE (identity_issuer IS NOT NULL AND identity_issuer != ?)
		   OR (blocked_issuer IS NOT NULL AND blocked_issuer != ?)`, issuer, issuer).Scan(&count)
	if err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("%w: %d account(s); restore the configured issuer or migrate explicitly (docs/IDENTITY.md)", ErrIdentityIssuerMismatch, count)
	}
	return nil
}

// CheckIdentity runs before consent; a new visitor leaves no account behind.
func (s *Store) CheckIdentity(ctx context.Context, id identity.Identity) (string, error) {
	if !identity.ValidSubject(id.Issuer, id.Subject) {
		return "", identity.ErrInvalid
	}
	u, err := s.ensureIdentity(ctx, id, false)
	return u.UID, err
}

// EnsureUser preserves the username's UID, slug and slot. Internal imports may
// omit both subject fields to create an unbound legacy row, never to bypass a binding.
func (s *Store) EnsureUser(ctx context.Context, id identity.Identity) (User, error) {
	return s.ensureIdentity(ctx, id, true)
}

func (s *Store) ensureIdentity(ctx context.Context, id identity.Identity, create bool) (User, error) {
	if !identity.ValidUID(id.UID) || id.Username == "" ||
		((id.Issuer != "" || id.Subject != "") && !identity.ValidSubject(id.Issuer, id.Subject)) {
		return User{}, identity.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, id.Username))
	if err == nil {
		var issuer, subject, blockedIssuer, blockedSubject sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT identity_issuer, identity_subject, blocked_issuer, blocked_subject FROM users WHERE uid = ?`, u.UID).
			Scan(&issuer, &subject, &blockedIssuer, &blockedSubject); err != nil {
			return User{}, err
		}
		if issuer.Valid != subject.Valid || blockedIssuer.Valid != blockedSubject.Valid {
			return User{}, identity.ErrInvalid
		}
		if (issuer.Valid || subject.Valid) && !identity.ValidSubject(id.Issuer, id.Subject) {
			return User{}, identity.ErrInvalid
		}
		if (issuer.Valid && issuer.String != id.Issuer) || (blockedIssuer.Valid && blockedIssuer.String != id.Issuer) {
			return User{}, ErrIdentityIssuerMismatch
		}
		if blockedSubject.Valid || (subject.Valid && subject.String != id.Subject) {
			// Latch the first conflicting subject; requests cannot change an admin's pending choice.
			if !blockedSubject.Valid {
				if _, err := tx.ExecContext(ctx, `UPDATE users SET blocked_issuer = ?, blocked_subject = ? WHERE uid = ?`, id.Issuer, id.Subject, u.UID); err != nil {
					return User{}, err
				}
			}
			if err := tx.Commit(); err != nil {
				return User{}, err
			}
			return User{}, &IdentityBlockedError{StoredHash: identity.SubjectHash(issuer.String, subject.String), IncomingHash: identity.SubjectHash(id.Issuer, id.Subject)}
		}
		if !issuer.Valid && !subject.Valid && id.Subject != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET identity_issuer = ?, identity_subject = ? WHERE uid = ?`, id.Issuer, id.Subject, u.UID); err != nil {
				return User{}, err
			}
		}
		if create && (u.Email != id.Email || u.DisplayName != id.Name) {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET email = ?, display_name = ? WHERE uid = ?`, id.Email, id.Name, u.UID); err != nil {
				return User{}, err
			}
			u.Email, u.DisplayName = id.Email, id.Name
		}
		return u, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return User{}, err
	}
	// A renamed account must not reach the previous username's data or consent.
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE uid = ?`, id.UID).Scan(&exists)
	if err == nil {
		h := sha256.Sum256([]byte(id.UID + "\x00" + id.Username))
		id.UID = fmt.Sprintf("%x", h[:])
	} else if !errors.Is(err, sql.ErrNoRows) {
		return User{}, err
	}
	if !create {
		return User{UID: id.UID}, nil
	}
	slug, err := freeSlug(ctx, tx, identity.Slugify(id.Username))
	if err != nil {
		return User{}, err
	}
	slot, err := freeSlot(ctx, tx)
	if err != nil {
		return User{}, err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (`+userCols+`, identity_issuer, identity_subject) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		id.UID, id.Username, id.Email, id.Name, slug, slot, now, null(id.Issuer), null(id.Subject)); err != nil {
		return User{}, err
	}
	u = User{UID: id.UID, Username: id.Username, Email: id.Email, DisplayName: id.Name, Slug: slug, Slot: slot, CreatedAt: time.UnixMilli(now)}
	return u, tx.Commit()
}

type IdentityBlock struct{ UID, Username, StoredHash, PendingHash string }

func (s *Store) IdentityBlocks(ctx context.Context) ([]IdentityBlock, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uid, username, identity_issuer, identity_subject, blocked_issuer, blocked_subject FROM users WHERE blocked_subject IS NOT NULL ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IdentityBlock
	for rows.Next() {
		var b IdentityBlock
		var issuer, subject, pendingIssuer, pendingSubject string
		if err := rows.Scan(&b.UID, &b.Username, &issuer, &subject, &pendingIssuer, &pendingSubject); err != nil {
			return nil, err
		}
		b.StoredHash = identity.SubjectHash(issuer, subject)
		b.PendingHash = identity.SubjectHash(pendingIssuer, pendingSubject)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ReleaseIdentity compares the form's candidate and audits in the same transaction.
func (s *Store) ReleaseIdentity(ctx context.Context, uid, pendingHash, adminUID string) error {
	if !identity.ValidUID(adminUID) {
		return identity.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var issuer, subject, pendingIssuer, pendingSubject string
	err = tx.QueryRowContext(ctx, `SELECT identity_issuer, identity_subject, blocked_issuer, blocked_subject FROM users WHERE uid = ? AND blocked_subject IS NOT NULL`, uid).
		Scan(&issuer, &subject, &pendingIssuer, &pendingSubject)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if identity.SubjectHash(pendingIssuer, pendingSubject) != pendingHash {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO identity_releases (ts, uid, admin_uid, old_subject_hash, new_subject_hash) VALUES (?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), uid, adminUID, identity.SubjectHash(issuer, subject), pendingHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET identity_issuer = blocked_issuer, identity_subject = blocked_subject, blocked_issuer = NULL, blocked_subject = NULL WHERE uid = ?`, uid); err != nil {
		return err
	}
	return tx.Commit()
}
