// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
)

// OpenCodeProfileProject excludes staged, isolated, deleted and trashed projects.
func (s *Store) OpenCodeAssistantProject(ctx context.Context, uid, id string) (Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects p
 WHERE p.uid=? AND p.id=? AND EXISTS (SELECT 1 FROM opencode_profile_projects oc
 WHERE oc.project_id=p.id AND oc.uid=p.uid AND oc.profile_hash=p.profile_hash)`, uid, id))
}

// PinProjectProfile stages a pin. Call through lifecycle.BindProjectProfile so
// the shared runtime is stopped first. Existing pins cannot be changed or cleared.
func (s *Store) PinProjectAssistant(ctx context.Context, uid, folder string, pin assistants.Pin) error {
	if err := pin.Validate(); err != nil {
		return err
	}
	if pin.ID == "" {
		return errors.New("profile: cannot clear a pin")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET profile_id = ?, profile_version = ?, profile_hash = ?
 WHERE uid = ? AND folder = ? AND (profile_id = '' OR (profile_id = ? AND profile_version = ? AND profile_hash = ?))`,
		pin.ID, pin.Version, pin.Hash, uid, folder, pin.ID, pin.Version, pin.Hash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SharedRuntimeAccess fails closed for staged pins, including trashed projects:
// their sessions still exist. There is no trusted per-project caller in this runtime.
func (s *Store) SharedRuntimeAccess(ctx context.Context, uid string) error {
	var blocked bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE uid = ? AND profile_id != '' AND id NOT IN (SELECT project_id FROM profile_projects) AND NOT EXISTS (SELECT 1 FROM opencode_profile_projects oc WHERE oc.project_id=projects.id AND oc.uid=projects.uid AND oc.profile_hash=projects.profile_hash))
 OR EXISTS(SELECT 1 FROM trash WHERE uid = ? AND profile_id != '' AND project_id NOT IN (SELECT project_id FROM profile_projects) AND NOT EXISTS (SELECT 1 FROM opencode_profile_projects oc WHERE oc.project_id=trash.project_id AND oc.uid=trash.uid AND oc.profile_hash=trash.profile_hash))`, uid, uid).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return assistants.ErrIsolationRequired
	}
	return nil
}

// CreateOpenCodeProfileProject admits only the newly created project; existing
// staged pins and isolated histories retain their execution boundary.
func (s *Store) CreateOpenCodeAssistantProject(ctx context.Context, p Project) error {
	if p.Assistant.ID == "" {
		return errors.New("assistant required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var admitted bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN consents c ON c.uid=u.uid WHERE u.uid=? AND u.blocked_subject IS NULL AND c.version=?)`, p.UID, consent.Version()).Scan(&admitted); err != nil {
		return err
	}
	if !admitted {
		return ErrNotFound
	}
	if err := insertProject(ctx, tx, p); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO opencode_profile_projects(project_id,uid,profile_hash) VALUES (?,?,?)`, p.ID, p.UID, p.Assistant.Hash); err != nil {
		return err
	}
	return tx.Commit()
}
