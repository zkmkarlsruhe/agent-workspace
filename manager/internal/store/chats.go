// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ManagerUID owns the usage of manager features that no person pays for
// (the chat evaluation): not in any person's totals or daily cap.
const ManagerUID = "manager"

// ChatQuota bounds stored snapshots per person; existing chats can still be updated.
const ChatQuota = 5000

var sessionID = regexp.MustCompile(`^ses_[0-9A-Za-z]{8,80}$`)

// ValidSessionID validates IDs reported by the runtime before storing or requesting chats.
func ValidSessionID(id string) bool { return sessionID.MatchString(id) }

// Chat is the stored snapshot of one root chat (lifecycle collects it).
type Chat struct {
	SessionID, UID, Project, Title string
	Created, Updated, CollectedAt  time.Time
	Messages, ToolCalls            int
	ToolErrors, Aborted            int
	ModelErrors                    int
	Truncated                      bool
	Transcript                     string // rendered like the export, bounded
}

// ChatEval is the stored evaluation of a chat in the state of ChatUpdated.
type ChatEval struct {
	ChatUpdated, EvaluatedAt time.Time
	Source                   string // llm | heuristik
	Result                   string // JSON of evaluation.Eval
}

// ChatRow is a chat with its person, cost and evaluation (nil = none).
type ChatRow struct {
	Chat
	Username, DisplayName string
	CostEUR               float64
	Eval                  *ChatEval
}

// ChatUpdates returns the stored updated time (unix ms) per chat of uid.
func (s *Store) ChatUpdates(ctx context.Context, uid string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, updated FROM chats WHERE uid = ?`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var ms int64
		if err := rows.Scan(&id, &ms); err != nil {
			return nil, err
		}
		out[id] = ms
	}
	return out, rows.Err()
}

// PutChat inserts or replaces the snapshot of (c.UID, c.SessionID), skipping
// invalid runtime IDs and new chats beyond ChatQuota.
func (s *Store) PutChat(ctx context.Context, c Chat) error {
	if !ValidSessionID(c.SessionID) {
		return nil
	}
	return s.putChat(ctx, c)
}

// PutAssistantChat accepts only manager-owned sessions; their IDs use a separate namespace.
func (s *Store) PutAssistantChat(ctx context.Context, c Chat) error {
	var owned bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM profile_sessions s JOIN projects p ON p.id = s.project_id AND p.uid = s.uid
		WHERE s.id = ? AND s.uid = ? AND p.folder = ?)`, c.SessionID, c.UID, c.Project).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return ErrNotFound
	}
	return s.putChat(ctx, c)
}

func (s *Store) putChat(ctx context.Context, c Chat) error {
	c.Title = strings.Join(strings.Fields(c.Title), " ")
	if len(c.Title) > 200 {
		n := 200
		for n > 0 && !utf8.RuneStart(c.Title[n]) {
			n--
		}
		c.Title = c.Title[:n]
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO chats (session_id, uid, project, title, created, updated, collected_at, first_collected,
		messages, tool_calls, tool_errors, aborted, model_errors, truncated, transcript)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
        FROM (SELECT ? AS uid, ? AS session_id, ? AS project) c WHERE `+chatPrivacySQL()+`
        AND (EXISTS (SELECT 1 FROM chats WHERE uid = c.uid AND session_id = c.session_id)
             OR (SELECT COUNT(*) FROM chats WHERE uid = c.uid) < ?)
        ON CONFLICT (uid, session_id) DO UPDATE SET project = excluded.project, title = excluded.title,
		created = excluded.created, updated = excluded.updated, collected_at = excluded.collected_at,
		messages = excluded.messages, tool_calls = excluded.tool_calls, tool_errors = excluded.tool_errors,
		aborted = excluded.aborted, model_errors = excluded.model_errors, truncated = excluded.truncated,
		transcript = excluded.transcript`,
		c.SessionID, c.UID, c.Project, c.Title, c.Created.UnixMilli(), c.Updated.UnixMilli(), c.CollectedAt.UnixMilli(), c.CollectedAt.UnixMilli(),
		c.Messages, c.ToolCalls, c.ToolErrors, c.Aborted, c.ModelErrors, c.Truncated, c.Transcript, c.UID, c.SessionID, c.Project, ChatQuota)
	return err
}

// chatSelect joins users, so anonymised stubs never show; tx is the
// transcript column or ” for lists.
func chatSelect(tx string) string {
	return `SELECT c.session_id, c.uid, c.project, c.title, c.created, c.updated, c.collected_at, c.messages, c.tool_calls,
		c.tool_errors, c.aborted, c.model_errors, c.truncated, ` + tx + `, u.username, u.display_name,
		(SELECT COALESCE(SUM(cost_eur), 0) FROM usage_events WHERE root_session_id = c.session_id AND uid = c.uid),
		e.chat_updated, e.evaluated_at, e.source, e.result
		FROM (SELECT * FROM chats c WHERE ` + chatPrivacySQL() + `) c JOIN users u ON u.uid = c.uid LEFT JOIN chat_evals e ON e.uid = c.uid AND e.session_id = c.session_id `
}

func scanChat(r scanner) (ChatRow, error) {
	var c ChatRow
	var created, updated, collected int64
	var eu, ea sql.NullInt64
	var es, er sql.NullString
	err := r.Scan(&c.SessionID, &c.UID, &c.Project, &c.Title, &created, &updated, &collected, &c.Messages, &c.ToolCalls,
		&c.ToolErrors, &c.Aborted, &c.ModelErrors, &c.Truncated, &c.Transcript, &c.Username, &c.DisplayName, &c.CostEUR,
		&eu, &ea, &es, &er)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.Created, c.Updated, c.CollectedAt = time.UnixMilli(created), time.UnixMilli(updated), time.UnixMilli(collected)
	if eu.Valid {
		c.Eval = &ChatEval{ChatUpdated: time.UnixMilli(eu.Int64), EvaluatedAt: time.UnixMilli(ea.Int64), Source: es.String, Result: er.String}
	}
	return c, err
}

func (s *Store) chatRows(ctx context.Context, q string, args ...any) ([]ChatRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatRow
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Chats lists chats last active in [from, to), newest first, without transcript.
func (s *Store) Chats(ctx context.Context, from, to time.Time, limit int) ([]ChatRow, error) {
	return s.chatRows(ctx, chatSelect("''")+`WHERE c.updated >= ? AND c.updated < ? ORDER BY c.updated DESC LIMIT ?`,
		from.UnixMilli(), to.UnixMilli(), limit)
}

// ChatByID returns one chat with transcript. A session id is unique per
// person only; if a container reported another person's id as well, the
// first collected snapshot wins (the forged copy can only come later). The
// order is the manager's first collection time, never the container-reported
// "created", which a forging container sets freely.
func (s *Store) ChatByID(ctx context.Context, id string) (ChatRow, error) {
	return scanChat(s.db.QueryRowContext(ctx, chatSelect("c.transcript")+`WHERE c.session_id = ? ORDER BY c.first_collected, c.uid LIMIT 1`, id))
}

// ChatsToEvaluate returns chats without evaluation or changed since, oldest
// change first, of people with an account (chatSelect joins users) who
// accepted the notice in version (consent.Version).
func (s *Store) ChatsToEvaluate(ctx context.Context, version string, limit int) ([]ChatRow, error) {
	return s.chatRows(ctx, chatSelect("c.transcript")+`WHERE (e.session_id IS NULL OR e.chat_updated < c.updated)
		AND EXISTS (SELECT 1 FROM consents k WHERE k.uid = c.uid AND k.version = ?)
		ORDER BY c.updated LIMIT ?`, version, limit)
}

// PutEval stores the evaluation of uid's chat in the state chatUpdated and
// reports whether it did. It stores nothing if the chat changed, its account
// was deleted or its consent in version (consent.Version) is gone while the
// evaluation ran: the result must not outlive the deletion or the withdrawal.
func (s *Store) PutEval(ctx context.Context, uid, id, version string, e ChatEval) (bool, error) {
	r, err := s.db.ExecContext(ctx, `INSERT INTO chat_evals (uid, session_id, chat_updated, evaluated_at, source, result)
		SELECT c.uid, c.session_id, ?, ?, ?, ? FROM chats c JOIN users u ON u.uid = c.uid
		JOIN consents k ON k.uid = c.uid AND k.version = ?
		WHERE c.uid = ? AND c.session_id = ? AND c.updated = ? AND `+chatPrivacySQL()+`
		ON CONFLICT (uid, session_id) DO UPDATE SET chat_updated = excluded.chat_updated,
		evaluated_at = excluded.evaluated_at, source = excluded.source, result = excluded.result`,
		e.ChatUpdated.UnixMilli(), e.EvaluatedAt.UnixMilli(), e.Source, e.Result, version, uid, id, e.ChatUpdated.UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// DayStat is the anonymous usage of one period. People and chats count
// deleted accounts too (their rows carry the "deleted-" or, from before the
// English rename, the "geloescht-" id).
type DayStat struct {
	People, Chats, ToolErrors int
	CostEUR, CO2LoG, CO2HiG   float64
	EvalCostEUR               float64 // ManagerUID: the evaluation itself
}

func (s *Store) DayStat(ctx context.Context, from, to time.Time) (DayStat, error) {
	var d DayStat
	f, t := from.UnixMilli(), to.UnixMilli()
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT uid), COUNT(DISTINCT root_session_id),
		COALESCE(SUM(cost_eur), 0), COALESCE(SUM(co2_g_lo), 0), COALESCE(SUM(co2_g_hi), 0)
		FROM usage_events WHERE ts_start >= ? AND ts_start < ? AND uid != ?`, f, t, ManagerUID).
		Scan(&d.People, &d.Chats, &d.CostEUR, &d.CO2LoG, &d.CO2HiG)
	if err != nil {
		return d, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_eur), 0) FROM usage_events WHERE ts_start >= ? AND ts_start < ? AND uid = ?`,
		f, t, ManagerUID).Scan(&d.EvalCostEUR); err != nil {
		return d, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(tool_errors), 0) FROM chats WHERE updated >= ? AND updated < ?`, f, t).Scan(&d.ToolErrors)
	return d, err
}

// AuditChatRead records that an admin read a chat.
func (s *Store) AuditChatRead(ctx context.Context, at time.Time, adminUID, adminName, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO admin_audit (ts, admin_uid, admin_name, session_id, action) VALUES (?, ?, ?, ?, 'chat_read')`,
		at.UnixMilli(), adminUID, adminName, sessionID)
	return err
}

// AuditEntry is one admin_audit row.
type AuditEntry struct {
	At                                 time.Time
	AdminUID, AdminName, SessionID, Do string
}

// Audit returns the newest audit rows first.
func (s *Store) Audit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts, admin_uid, admin_name, session_id, action FROM admin_audit ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var a AuditEntry
		var ms int64
		if err := rows.Scan(&ms, &a.AdminUID, &a.AdminName, &a.SessionID, &a.Do); err != nil {
			return nil, err
		}
		a.At = time.UnixMilli(ms)
		out = append(out, a)
	}
	return out, rows.Err()
}
