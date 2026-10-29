// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/settings"
)

var _ settings.Store = (*Store)(nil)

// LoadProfile returns the stored profile (migration 8) with the portal
// language (migration 11); unknown uid = zero value.
func (s *Store) LoadSettings(ctx context.Context, uid string) (settings.Record, error) {
	var d settings.Record
	var facts, text sql.NullString
	var answers sql.NullString
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT u.lang, p.facts, p.answers, p.text, p.updated_at
		FROM users u LEFT JOIN profiles p ON p.uid = u.uid WHERE u.uid = ?`, uid).
		Scan(&d.Lang, &facts, &answers, &text, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return d, nil
	}
	if err != nil || !facts.Valid {
		return d, err
	}
	d.Text = text.String
	if err := json.Unmarshal([]byte(facts.String), &d.Facts); err != nil {
		return d, err
	}
	if answers.Valid {
		d.Answers = new(settings.Answers)
		if err := json.Unmarshal([]byte(answers.String), d.Answers); err != nil {
			return d, err
		}
	}
	if at.Int64 != 0 {
		d.Modified = time.UnixMilli(at.Int64)
	}
	return d, nil
}

func (s *Store) SaveFacts(ctx context.Context, uid string, f settings.Facts) error {
	b, _ := json.Marshal(f)
	_, err := s.db.ExecContext(ctx, `INSERT INTO profiles (uid, facts) VALUES (?, ?)
		ON CONFLICT (uid) DO UPDATE SET facts = excluded.facts`, uid, string(b))
	return err
}

func (s *Store) SaveSettings(ctx context.Context, uid string, a settings.Answers, text string, at time.Time) error {
	b, _ := json.Marshal(a)
	_, err := s.db.ExecContext(ctx, `INSERT INTO profiles (uid, answers, text, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (uid) DO UPDATE SET answers = excluded.answers, text = excluded.text, updated_at = excluded.updated_at`,
		uid, string(b), text, at.UnixMilli())
	return err
}
