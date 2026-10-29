// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrLimit: the person already sent the maximum of feedback in the window.
var ErrLimit = errors.New("store: feedback limit reached")

// Feedback is one message to the operator (migration 17). Source is "chat"
// (MCP tool feedback_send) or "portal" (form).
type Feedback struct {
	ID                          int64
	At                          time.Time
	UID, Source, Kind, Text     string
	Project, SessionID, Version string
	Context                     string // rendered chat tail; FeedbackList leaves it empty
	Done                        bool
	Username, DisplayName       string // joined from users
}

// AddFeedback stores f unless uid already has max rows since since.
func (s *Store) AddFeedback(ctx context.Context, f Feedback, since time.Time, max int) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback WHERE uid = ? AND ts >= ?`, f.UID, since.UnixMilli()).Scan(&n); err != nil {
		return 0, err
	}
	if n >= max {
		return 0, ErrLimit
	}
	// the excerpt was captured before the insert: a session deleted for good meanwhile keeps it out
	res, err := tx.ExecContext(ctx, `INSERT INTO feedback (ts, uid, source, kind, text, project, session_id, version, context)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, CASE WHEN EXISTS (SELECT 1 FROM deleted_sessions WHERE uid = ? AND session_id = ?) THEN '' ELSE ? END`,
		f.At.UnixMilli(), f.UID, f.Source, f.Kind, f.Text, f.Project, f.SessionID, f.Version, f.UID, f.SessionID, f.Context)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, tx.Commit()
}

func feedbackSelect(ctxCol string) string {
	return `SELECT f.id, f.ts, f.uid, f.source, f.kind, f.text, f.project, f.session_id, f.version, ` + ctxCol + `, f.done,
		COALESCE(u.username, ''), COALESCE(u.display_name, '') FROM feedback f LEFT JOIN users u ON u.uid = f.uid `
}

func scanFeedback(r scanner) (Feedback, error) {
	var f Feedback
	var ms int64
	err := r.Scan(&f.ID, &ms, &f.UID, &f.Source, &f.Kind, &f.Text, &f.Project, &f.SessionID, &f.Version, &f.Context, &f.Done,
		&f.Username, &f.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	f.At = time.UnixMilli(ms)
	return f, err
}

// FeedbackList returns the newest feedback first, without context.
func (s *Store) FeedbackList(ctx context.Context, limit int) ([]Feedback, error) {
	rows, err := s.db.QueryContext(ctx, feedbackSelect("''")+`ORDER BY f.ts DESC, f.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feedback
	for rows.Next() {
		f, err := scanFeedback(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FeedbackByID returns one feedback with context.
func (s *Store) FeedbackByID(ctx context.Context, id int64) (Feedback, error) {
	return scanFeedback(s.db.QueryRowContext(ctx, feedbackSelect("f.context")+`WHERE f.id = ?`, id))
}

// SetFeedbackDone marks a feedback as handled (or not); ErrNotFound if it is gone.
func (s *Store) SetFeedbackDone(ctx context.Context, id int64, done bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE feedback SET done = ? WHERE id = ?`, done, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AuditFeedbackRead records that an admin opened a feedback with its chat excerpt.
func (s *Store) AuditFeedbackRead(ctx context.Context, at time.Time, adminUID, adminName string, id int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin_audit (ts, admin_uid, admin_name, session_id, action) VALUES (?, ?, ?, ?, 'feedback_read')`,
		at.UnixMilli(), adminUID, adminName, fmt.Sprintf("feedback-%d", id))
	return err
}
