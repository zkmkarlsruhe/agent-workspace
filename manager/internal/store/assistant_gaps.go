// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

const AssistantGapRetention = 30 * 24 * time.Hour

type AssistantGap struct {
	ID, UID, ProjectID, Assistant, Summary, Sources, Category, Guidance string
	Revision                                                            int
	Created, Expires                                                    time.Time
}

func (g AssistantGap) Validate() error {
	if _, err := uuid.Parse(g.ID); err != nil {
		return err
	}
	if !slices.Contains([]string{"missing", "outdated", "unclear"}, g.Category) {
		return errors.New("invalid gap category")
	}
	for _, text := range []string{g.Summary, g.Sources} {
		if !utf8.ValidString(text) || len(text) > 4000 || strings.ContainsRune(text, '\x00') {
			return errors.New("invalid gap text")
		}
	}
	if strings.TrimSpace(g.Summary) == "" {
		return errors.New("gap summary is required")
	}
	return nil
}

// AddAssistantGap accepts only a person's reviewed summary, never a transcript.
func (s *Store) AddAssistantGap(ctx context.Context, g AssistantGap) error {
	if err := g.Validate(); err != nil {
		return err
	}
	p, err := scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects p WHERE p.uid=? AND p.id=?`, g.UID, g.ProjectID))
	if err != nil {
		return err
	}
	def, ok := instance.Current().Assistant(p.Assistant.ID)
	if !ok || !def.GapCollection || !def.Enabled || instance.Current().AssistantPin(def) != p.Assistant {
		return ErrNotFound
	}
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM assistant_gaps WHERE id=? AND uid=? AND project_id=? AND summary=? AND sources=? AND category=?)`, g.ID, g.UID, g.ProjectID, g.Summary, g.Sources, g.Category).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM assistant_gaps WHERE uid=? AND created>?`, g.UID, now.Add(-24*time.Hour).UnixMilli()).Scan(&count); err != nil {
		return err
	}
	if count >= 10 {
		return ErrLimit
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO assistant_gaps (id,uid,project_id,assistant,summary,sources,category,created,expires)
 SELECT ?,p.uid,p.id,p.profile_id,?,?,?,?,? FROM projects p JOIN consents c ON c.uid=p.uid JOIN users u ON u.uid=p.uid
 WHERE p.id=? AND p.uid=? AND p.profile_hash=? AND c.version=? AND u.blocked_subject IS NULL`, g.ID, g.Summary, g.Sources, g.Category, now.UnixMilli(), now.Add(AssistantGapRetention).UnixMilli(), g.ProjectID, g.UID, p.Assistant.Hash, consent.Version())
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return tx.Commit()
}

// uid empty is reserved for authenticated admin callers. Expired rows are never returned.
func (s *Store) AssistantGaps(ctx context.Context, uid, project string) ([]AssistantGap, error) {
	limit := -1
	if uid == "" {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,uid,project_id,assistant,summary,sources,category,guidance,revision,created,expires FROM assistant_gaps WHERE expires>? AND (?='' OR uid=?) AND (?='' OR project_id=?) ORDER BY created DESC LIMIT ?`, time.Now().UnixMilli(), uid, uid, project, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AssistantGap
	for rows.Next() {
		var g AssistantGap
		var created, expires int64
		if err := rows.Scan(&g.ID, &g.UID, &g.ProjectID, &g.Assistant, &g.Summary, &g.Sources, &g.Category, &g.Guidance, &g.Revision, &created, &expires); err != nil {
			return nil, err
		}
		g.Created = time.UnixMilli(created)
		g.Expires = time.UnixMilli(expires)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAssistantGap(ctx context.Context, uid, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM assistant_gaps WHERE uid=? AND id=?`, uid, id)
	return err
}

func (s *Store) AnswerAssistantGap(ctx context.Context, actor, id string, revision int, guidance string) error {
	if guidance != "" {
		u, err := url.Parse(guidance)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(guidance) > 2000 || strings.ContainsAny(guidance, "\r\n\x00") {
			return errors.New("guidance must be an HTTPS link")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE assistant_gaps SET guidance=?,revision=revision+1 WHERE id=? AND revision=? AND expires>?`, guidance, id, revision, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n != 1 {
		return ErrAssistantConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_audit (ts,admin_uid,admin_name,session_id,action) VALUES (?,?,'',?,'assistant_gap_answer')`, time.Now().UnixMilli(), actor, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PurgeAssistantGaps(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM assistant_gaps WHERE expires<=?`, now.UnixMilli())
	return err
}
