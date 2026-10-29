// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/assistants"
)

type Project struct {
	// nil inherits the migration default; an empty slice explicitly selects none.
	Capabilities                 []string
	Isolated                     bool
	Assistant                    assistants.Pin
	ID, UID, Folder, Description string
	CreatedAt                    time.Time
	// LastActivity is the newest of creation, recorded portal and chat
	// activity (TouchProject) and the project's metered LLM calls. Read-only,
	// except that an insert starts with it (RestoreTrash).
	LastActivity time.Time
}

var ErrExists = errors.New("store: already exists")

// lastActivity is the effective last activity of project row p.
const lastActivity = `MAX(p.created_at, p.last_activity,
	COALESCE((SELECT MAX(u.ts_start) FROM usage_events u WHERE u.project_id = p.id AND u.uid = p.uid), 0))`

const projectCols = `p.id, p.uid, p.folder, p.description, p.capabilities, p.created_at, p.profile_id, p.profile_version, p.profile_hash, EXISTS(SELECT 1 FROM profile_projects pp WHERE pp.project_id = p.id AND pp.uid = p.uid), ` + lastActivity

func scanProject(r scanner) (Project, error) {
	var p Project
	var created, last int64
	var capabilities string
	err := r.Scan(&p.ID, &p.UID, &p.Folder, &p.Description, &capabilities, &created, &p.Assistant.ID, &p.Assistant.Version, &p.Assistant.Hash, &p.Isolated, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal([]byte(capabilities), &p.Capabilities)
	}
	p.CreatedAt, p.LastActivity = time.UnixMilli(created), time.UnixMilli(last)
	return p, err
}

func (s *Store) projects(ctx context.Context, where string, args ...any) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects p WHERE `+where+` ORDER BY p.created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) CreateProject(ctx context.Context, p Project) error {
	return insertProject(ctx, s.db, p)
}

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func insertProject(ctx context.Context, db execer, p Project) error {
	if err := p.Assistant.Validate(); err != nil {
		return err
	}
	last := max(p.LastActivity.UnixMilli(), p.CreatedAt.UnixMilli())
	capabilities, err := json.Marshal(p.Capabilities)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, uid, folder, description, created_at, last_activity, profile_id, profile_version, profile_hash, capabilities) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.UID, p.Folder, p.Description, p.CreatedAt.UnixMilli(), last, p.Assistant.ID, p.Assistant.Version, p.Assistant.Hash, string(capabilities))
	if err != nil && isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *Store) DeleteProject(ctx context.Context, uid, folder string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE uid = ? AND folder = ?`, uid, folder)
	return err
}

// RenameProject changes the folder of project id; ErrExists if the uid has a
// project of that name.
func (s *Store) RenameProject(ctx context.Context, id, folder string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET folder = ? WHERE id = ?`, folder, id)
	if err != nil {
		if isUnique(err) {
			return ErrExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Projects(ctx context.Context, uid string) ([]Project, error) {
	return s.projects(ctx, `p.uid = ?`, uid)
}

func (s *Store) ProjectByFolder(ctx context.Context, uid, folder string) (Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects p WHERE p.uid = ? AND p.folder = ?`, uid, folder))
}

// TouchProject records activity on a project at t; an older t changes nothing.
func (s *Store) TouchProject(ctx context.Context, uid, folder string, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET last_activity = MAX(last_activity, ?) WHERE uid = ? AND folder = ?`,
		t.UnixMilli(), uid, folder)
	return err
}

// IdleProjects returns the projects of all people whose last activity is before t.
func (s *Store) IdleProjects(ctx context.Context, before time.Time) ([]Project, error) {
	return s.projects(ctx, lastActivity+` < ?`, before.UnixMilli())
}

// NameTaken reports whether folder is used by a project or a trash item.
func (s *Store) NameTaken(ctx context.Context, uid, folder string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM projects WHERE uid = ? AND folder = ?)
		+ (SELECT COUNT(*) FROM trash WHERE uid = ? AND folder = ?)`, uid, folder, uid, folder).Scan(&n)
	return n > 0, err
}

type TrashItem struct {
	ID         string
	Project    Project
	DeletedAt  time.Time
	PurgeAfter time.Time
	Chats      int  // stored root sessions
	Auto       bool // moved by the inactivity rule, not by the person
	// Pending: the folder still sits at /workspace/<folder>; it moves into
	// the trash dir the next time the environment runs (FinishPending).
	Pending bool
}

// TrashSession is an OpenCode session that belongs to a trash item.
type TrashSession struct {
	ID   string
	Root bool
}

const reasonIdle = "idle"

// MoveToTrash replaces the project row by a trash row in one transaction and
// records the project's sessions plus any session metered for the project.
// auto marks a move by the inactivity rule; pending says the folder has not
// moved yet (environment stopped).
func (s *Store) MoveToTrash(ctx context.Context, p Project, id string, now, purge time.Time, sessions []TrashSession, auto, pending bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var capabilities string
	// A staging operation may have pinned the project after the caller read it.
	if err := tx.QueryRowContext(ctx, `SELECT profile_id, profile_version, profile_hash, capabilities FROM projects WHERE id = ? AND uid = ?`, p.ID, p.UID).Scan(&p.Assistant.ID, &p.Assistant.Version, &p.Assistant.Hash, &capabilities); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, p.ID); err != nil {
		return err
	}
	reason := ""
	if auto {
		reason = reasonIdle
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO trash (id, uid, project_id, folder, description, created_at, deleted_at, purge_after, reason, pending_move, profile_id, profile_version, profile_hash, capabilities)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, p.UID, p.ID, p.Folder, p.Description, p.CreatedAt.UnixMilli(), now.UnixMilli(), purge.UnixMilli(),
		reason, pending, p.Assistant.ID, p.Assistant.Version, p.Assistant.Hash, capabilities); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO trash_sessions(trash_id,session_id,root) SELECT ?,id,1 FROM profile_sessions WHERE uid=? AND project_id=?`, id, p.UID, p.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE profile_sessions SET busy_until=0,revision=revision+1 WHERE uid=? AND project_id=?`, p.UID, p.ID); err != nil {
		return err
	}
	if err := addTrashSessions(ctx, tx, id, sessions); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO trash_sessions (trash_id, session_id, root)
		SELECT DISTINCT ?, session_id, 0 FROM usage_events WHERE uid = ? AND project_id = ? AND session_id IS NOT NULL`,
		id, p.UID, p.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func addTrashSessions(ctx context.Context, db execer, id string, sessions []TrashSession) error {
	for _, se := range sessions {
		// a root found later (FinishPending) upgrades a metered id stored as non-root
		if _, err := db.ExecContext(ctx, `INSERT INTO trash_sessions (trash_id, session_id, root) VALUES (?, ?, ?)
			ON CONFLICT DO UPDATE SET root = max(root, excluded.root)`, id, se.ID, se.Root); err != nil {
			return err
		}
	}
	return nil
}

// FinishPending records the sessions found in the folder of a pending trash
// item after it moved into the trash dir, and clears the mark.
func (s *Store) FinishPending(ctx context.Context, id string, sessions []TrashSession) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := addTrashSessions(ctx, tx, id, sessions); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE trash SET pending_move = 0 WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// TrashSessions returns the sessions recorded for a trash item.
func (s *Store) TrashSessions(ctx context.Context, trashID string) ([]TrashSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, root FROM trash_sessions WHERE trash_id = ? ORDER BY session_id`, trashID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrashSession
	for rows.Next() {
		var se TrashSession
		if err := rows.Scan(&se.ID, &se.Root); err != nil {
			return nil, err
		}
		out = append(out, se)
	}
	return out, rows.Err()
}

const trashCols = `id, uid, project_id, folder, description, capabilities, created_at, deleted_at, purge_after,
	(SELECT COUNT(*) FROM trash_sessions WHERE trash_id = trash.id AND root = 1), reason = '` + reasonIdle + `', pending_move, profile_id, profile_version, profile_hash, EXISTS(SELECT 1 FROM profile_projects pp WHERE pp.project_id = trash.project_id AND pp.uid = trash.uid)`

func scanTrash(r scanner) (TrashItem, error) {
	var t TrashItem
	var c, d, p int64
	var capabilities string
	err := r.Scan(&t.ID, &t.Project.UID, &t.Project.ID, &t.Project.Folder, &t.Project.Description, &capabilities, &c, &d, &p, &t.Chats, &t.Auto, &t.Pending, &t.Project.Assistant.ID, &t.Project.Assistant.Version, &t.Project.Assistant.Hash, &t.Project.Isolated)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal([]byte(capabilities), &t.Project.Capabilities)
	}
	t.Project.CreatedAt, t.DeletedAt, t.PurgeAfter = time.UnixMilli(c), time.UnixMilli(d), time.UnixMilli(p)
	return t, err
}

func (s *Store) Trash(ctx context.Context, uid string) ([]TrashItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+trashCols+` FROM trash WHERE uid = ? ORDER BY deleted_at DESC`, uid)
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

func (s *Store) TrashItem(ctx context.Context, uid, id string) (TrashItem, error) {
	return scanTrash(s.db.QueryRowContext(ctx, `SELECT `+trashCols+` FROM trash WHERE uid = ? AND id = ?`, uid, id))
}

// RestoreTrash turns a trash row back into a project named folder, active at
// now: otherwise the inactivity rule would move it back at its next run.
func (s *Store) RestoreTrash(ctx context.Context, t TrashItem, folder string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `DELETE FROM trash WHERE id = ? AND uid = ?`, t.ID, t.Project.UID)
	if err := changed(r, err); err != nil {
		return err
	}
	p := t.Project
	p.Folder, p.LastActivity = folder, now
	if err := insertProject(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteTrash removes a trash item for good. The collected chat snapshots of
// its sessions lose title, project and transcript (counts stay for the daily
// figures) and their evaluations go, like on account deletion.
func (s *Store) DeleteTrash(ctx context.Context, uid, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// session ids are reported by the container: only uid's own rows, via uid's own trash item
	const sessions = `SELECT ts.session_id FROM trash_sessions ts JOIN trash t ON t.id = ts.trash_id WHERE t.id = ? AND t.uid = ?`
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_evals WHERE uid = ? AND session_id IN (`+sessions+`)`, uid, id, uid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chats SET project = '', title = '', transcript = '' WHERE uid = ? AND session_id IN (`+sessions+`)`, uid, id, uid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM opencode_profile_projects WHERE uid = ? AND project_id IN (SELECT project_id FROM trash WHERE uid = ? AND id = ?)`, uid, uid, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM profile_projects WHERE uid = ? AND project_id IN (SELECT project_id FROM trash WHERE uid = ? AND id = ?)`, uid, uid, id); err != nil {
		return err
	}
	for _, table := range []string{"nc_links", "nc_collections"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE project_id IN (SELECT project_id FROM trash WHERE uid = ? AND id = ?)`, uid, id); err != nil {
			return err
		}
	}
	// the person's own words to the operator stay; the chat excerpt goes with the chats
	if _, err := tx.ExecContext(ctx, `UPDATE feedback SET context = '' WHERE uid = ? AND session_id IN (`+sessions+`)`, uid, id, uid); err != nil {
		return err
	}
	// the marker AddFeedback checks: a feedback captured before this commit stays without excerpt
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO deleted_sessions (uid, session_id) SELECT ?, session_id FROM (`+sessions+`)`, uid, id, uid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM trash WHERE uid = ? AND id = ?`, uid, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Deleted counts what DeleteUser removed or anonymised.
type Deleted struct {
	Projects, Trash, UsageEvents, Chats int64
}

// DeleteUser removes the person's rows: projects, trash items with their
// sessions, and the user, which frees slug and slot. Usage events stay under
// anon, so cost and CO2 totals remain right without pointing to the person.
// Chat snapshots lose title, project and transcript and move to anon too;
// their evaluations and the person's feedback are deleted.
func (s *Store) DeleteUser(ctx context.Context, uid, anon string) (Deleted, error) {
	var d Deleted
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return d, err
	}
	defer tx.Rollback()
	ledgerFrom := time.Now().Add(-48 * time.Hour).UnixMilli()
	steps := []struct {
		n    *int64
		sql  string
		args []any
	}{
		{&d.Projects, `DELETE FROM projects WHERE uid = ?`, []any{uid}},
		{&d.Trash, `DELETE FROM trash WHERE uid = ?`, []any{uid}}, // trash_sessions cascade
		// the daily cap must survive the deletion: keep the last two days' spend under the uid
		{new(int64), `DELETE FROM spend_ledger WHERE ts < ?`, []any{ledgerFrom}},
		{new(int64), `INSERT INTO spend_ledger (uid, ts, cost_eur) SELECT uid, ts_start, cost_eur FROM usage_events
			WHERE uid = ? AND ts_start >= ? AND cost_eur IS NOT NULL`, []any{uid, ledgerFrom}},
		{&d.UsageEvents, `UPDATE usage_events SET uid = ? WHERE uid = ?`, []any{anon, uid}},
		{new(int64), `DELETE FROM chat_evals WHERE uid = ?`, []any{uid}},
		// the stub keeps the counts of the daily overview, nothing that points to the person
		{&d.Chats, `UPDATE chats SET uid = ?, project = '', title = '', transcript = '' WHERE uid = ?`, []any{anon, uid}},
		{new(int64), `DELETE FROM consents WHERE uid = ?`, []any{uid}},
		{new(int64), `DELETE FROM feedback WHERE uid = ?`, []any{uid}},
		{new(int64), `DELETE FROM deleted_sessions WHERE uid = ?`, []any{uid}},
		{new(int64), `DELETE FROM nc_uploads WHERE uid = ?`, []any{uid}},
		{new(int64), `DELETE FROM users WHERE uid = ?`, []any{uid}},
	}
	for _, st := range steps {
		res, err := tx.ExecContext(ctx, st.sql, st.args...)
		if err != nil {
			return d, err
		}
		*st.n, _ = res.RowsAffected()
	}
	return d, tx.Commit()
}

// UsageRow is a usage sum. Sessions counts chats: a subagent call counts
// for its root session.
type UsageRow struct {
	ProjectID      string // Usage only
	SessionID      string // SessionUsage only (root session = chat)
	CostEUR        float64
	CO2LoG, CO2HiG float64
	Sessions       int
}

const usageSums = `COALESCE(SUM(cost_eur), 0), COALESCE(SUM(co2_g_lo), 0), COALESCE(SUM(co2_g_hi), 0),
	COUNT(DISTINCT COALESCE(root_session_id, session_id))`

func (s *Store) usageRows(ctx context.Context, q string, args ...any) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.ProjectID, &u.SessionID, &u.CostEUR, &u.CO2LoG, &u.CO2HiG, &u.Sessions); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Usage sums usage events of uid per project since t.
func (s *Store) Usage(ctx context.Context, uid string, since time.Time) ([]UsageRow, error) {
	return s.usageRows(ctx, `SELECT COALESCE(project_id, ''), '', `+usageSums+`
		FROM usage_events WHERE uid = ? AND ts_start >= ? GROUP BY project_id`, uid, since.UnixMilli())
}

// SessionUsage sums usage per chat (root session) of one project since t.
func (s *Store) SessionUsage(ctx context.Context, uid, projectID string, since time.Time) ([]UsageRow, error) {
	return s.usageRows(ctx, `SELECT project_id, COALESCE(root_session_id, session_id), `+usageSums+`
		FROM usage_events WHERE uid = ? AND project_id = ? AND ts_start >= ? AND COALESCE(root_session_id, session_id) IS NOT NULL
		GROUP BY COALESCE(root_session_id, session_id)`, uid, projectID, since.UnixMilli())
}

// ChatUsage sums the usage of one chat (root session, subagents included) of
// uid since t.
func (s *Store) ChatUsage(ctx context.Context, uid, session string, since time.Time) (UsageRow, error) {
	rows, err := s.usageRows(ctx, `SELECT '', '', `+usageSums+` FROM usage_events
		WHERE uid = ? AND COALESCE(root_session_id, session_id) = ? AND ts_start >= ?`, uid, session, since.UnixMilli())
	if err != nil || len(rows) == 0 {
		return UsageRow{}, err
	}
	return rows[0], nil
}

// UsageTotal sums all usage of uid since t.
func (s *Store) UsageTotal(ctx context.Context, uid string, since time.Time) (UsageRow, error) {
	rows, err := s.usageRows(ctx, `SELECT '', '', `+usageSums+` FROM usage_events WHERE uid = ? AND ts_start >= ?`,
		uid, since.UnixMilli())
	if err != nil || len(rows) == 0 {
		return UsageRow{}, err
	}
	return rows[0], nil
}
