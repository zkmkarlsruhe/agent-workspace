// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
)

// AssistantSession is private conversation storage, never a collected evaluation chat.
type AssistantSession struct {
	ID, UID, ProjectID string
	Messages           json.RawMessage
	Revision           int
	Updated            time.Time
}

func (s *Store) CreateAssistantProject(ctx context.Context, p Project, evaluate bool) error {
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
	_, err = tx.ExecContext(ctx, `INSERT INTO profile_projects(project_id, uid, consent_hash, evaluation) VALUES (?, ?, ?, ?)`, p.ID, p.UID, p.Assistant.Hash, evaluate)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateAssistantSession(ctx context.Context, uid, project, id string, now time.Time) error {
	r, err := s.db.ExecContext(ctx, `INSERT INTO profile_sessions(id, uid, project_id, updated)
 SELECT ?, p.uid, p.id, ? FROM projects p JOIN profile_projects pp ON pp.project_id = p.id AND pp.uid = p.uid
 WHERE p.uid = ? AND p.id = ? AND pp.consent_hash = p.profile_hash`, id, now.UnixMilli(), uid, project)
	return changed(r, err)
}

func changed(r sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return err
}

func scanAssistantSession(r scanner) (AssistantSession, error) {
	var se AssistantSession
	var updated int64
	var messages string
	err := r.Scan(&se.ID, &se.UID, &se.ProjectID, &messages, &se.Revision, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	se.Messages = json.RawMessage(messages)
	se.Updated = time.UnixMilli(updated)
	return se, err
}

const assistantSessionCols = `s.id, s.uid, s.project_id, s.messages, s.revision, s.updated`

func (s *Store) AssistantSession(ctx context.Context, uid, id string) (AssistantSession, error) {
	return scanAssistantSession(s.db.QueryRowContext(ctx, `SELECT `+assistantSessionCols+` FROM profile_sessions s JOIN projects p ON p.id = s.project_id AND p.uid = s.uid WHERE s.uid = ? AND s.id = ?`, uid, id))
}

func (s *Store) AssistantSessions(ctx context.Context, uid, project string) ([]AssistantSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assistantSessionCols+` FROM profile_sessions s WHERE s.uid = ? AND s.project_id = ? ORDER BY s.updated DESC`, uid, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssistantSession
	for rows.Next() {
		se, err := scanAssistantSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, se)
	}
	return out, rows.Err()
}

// ProfileAccount is captured for one bounded turn. A rebind, blocked identity,
// deletion/recreation or generation change revokes that turn.
func (s *Store) AssistantAccount(ctx context.Context, uid string) (string, error) {
	var stamp string
	err := s.db.QueryRowContext(ctx, `SELECT json_array(created_at, generation, identity_issuer, identity_subject) FROM users WHERE uid = ? AND blocked_subject IS NULL`, uid).Scan(&stamp)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return stamp, err
}

func (s *Store) AssistantProjectByID(ctx context.Context, uid, id string) (Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects p JOIN profile_projects pp ON pp.project_id = p.id AND pp.uid = p.uid WHERE p.uid = ? AND p.id = ? AND pp.consent_hash = p.profile_hash`, uid, id))
}

// ClaimProfileTurn serializes paid turns, including across requests/restarts.
func (s *Store) ClaimAssistantTurn(ctx context.Context, se AssistantSession, until time.Time) error {
	r, err := s.db.ExecContext(ctx, `UPDATE profile_sessions SET busy_until = ? WHERE uid = ? AND id = ? AND revision = ? AND busy_until < ?`, until.UnixMilli(), se.UID, se.ID, se.Revision, time.Now().UnixMilli())
	return changed(r, err)
}

func (s *Store) ReleaseAssistantTurn(ctx context.Context, se AssistantSession, until time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE profile_sessions SET busy_until = 0 WHERE uid = ? AND id = ? AND revision = ? AND busy_until = ?`, se.UID, se.ID, se.Revision, until.UnixMilli())
	return err
}

// CommitProfileTurn repeats ownership, consent, pin, account and lease checks in
// the write itself: a result cannot resurrect a deleted/trashed/revoked session.
func (s *Store) CommitAssistantTurn(ctx context.Context, se AssistantSession, pin assistants.Pin, account, consent string, until time.Time, messages []byte) error {
	r, err := s.db.ExecContext(ctx, `UPDATE profile_sessions SET messages = ?, revision = revision + 1, updated = ?, busy_until = 0
 WHERE uid = ? AND id = ? AND revision = ? AND busy_until = ? AND busy_until > ?
 AND EXISTS (SELECT 1 FROM projects p JOIN profile_projects pp ON pp.project_id = p.id AND pp.uid = p.uid
 JOIN users u ON u.uid = p.uid JOIN consents c ON c.uid = u.uid
 WHERE p.id = profile_sessions.project_id AND p.uid = profile_sessions.uid AND p.profile_id = ? AND p.profile_version = ? AND p.profile_hash = ? AND pp.consent_hash = p.profile_hash
 AND u.blocked_subject IS NULL AND json_array(u.created_at, u.generation, u.identity_issuer, u.identity_subject) = ? AND c.version = ?)`,
		messages, time.Now().UnixMilli(), se.UID, se.ID, se.Revision, until.UnixMilli(), time.Now().UnixMilli(), pin.ID, pin.Version, pin.Hash, account, consent)
	return changed(r, err)
}

func (s *Store) AssistantEvaluationOptIn(ctx context.Context, uid, project string) (bool, error) {
	var on bool
	err := s.db.QueryRowContext(ctx, `SELECT evaluation FROM profile_projects WHERE uid = ? AND project_id = ?`, uid, project).Scan(&on)
	return on, err
}

func (s *Store) WithdrawAssistantEvaluation(ctx context.Context, uid, project string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE profile_projects SET evaluation = 0 WHERE uid = ? AND project_id = ?`, uid, project); err != nil {
		return err
	}
	for _, table := range []string{"chat_evals", "chats"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE uid = ? AND session_id IN (SELECT id FROM profile_sessions WHERE uid = ? AND project_id = ?)`, uid, uid, project); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ExpiredAssistantTrash(ctx context.Context, now time.Time) ([]TrashItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+trashCols+` FROM trash WHERE purge_after<=? AND project_id IN (SELECT project_id FROM profile_projects)`, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrashItem
	for rows.Next() {
		t, err := scanTrash(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
