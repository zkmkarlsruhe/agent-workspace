// SPDX-License-Identifier: MIT

// Package store keeps the manager state in SQLite (pure-Go driver, WAL).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("store: not found")

// migrations run forward only; index+1 is the PRAGMA user_version after it.
var migrations = []string{`
CREATE TABLE users (
	uid          TEXT PRIMARY KEY CHECK (length(uid) = 64),
	username     TEXT NOT NULL,
	email        TEXT NOT NULL DEFAULT '',
	display_name TEXT NOT NULL DEFAULT '',
	slug         TEXT NOT NULL UNIQUE,
	slot         INTEGER NOT NULL UNIQUE,
	generation   INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL
);
CREATE TABLE projects (
	id          TEXT PRIMARY KEY,
	uid         TEXT NOT NULL REFERENCES users(uid),
	folder      TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	UNIQUE (uid, folder)
);
CREATE TABLE trash (
	id          TEXT PRIMARY KEY,
	uid         TEXT NOT NULL REFERENCES users(uid),
	project_id  TEXT NOT NULL,
	folder      TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	deleted_at  INTEGER NOT NULL,
	purge_after INTEGER NOT NULL
);
CREATE TABLE usage_events (
	event_id         TEXT PRIMARY KEY,
	ts_start         INTEGER NOT NULL,
	ts_end           INTEGER NOT NULL,
	uid              TEXT NOT NULL,
	project_id       TEXT,
	session_id       TEXT,
	capability       TEXT NOT NULL,
	target           TEXT,
	deployment_id    TEXT,
	status           INTEGER NOT NULL,
	finish_reason    TEXT,
	in_tokens        INTEGER,
	cached_tokens    INTEGER,
	out_tokens       INTEGER,
	reasoning_tokens INTEGER,
	cost_eur         REAL,
	cost_source      TEXT
);
CREATE INDEX usage_events_uid_ts ON usage_events (uid, ts_start);
CREATE INDEX usage_events_project_ts ON usage_events (project_id, ts_start);
CREATE INDEX usage_events_session ON usage_events (session_id);
`, `
ALTER TABLE usage_events ADD COLUMN root_session_id TEXT;
ALTER TABLE usage_events ADD COLUMN wh_lo REAL;
ALTER TABLE usage_events ADD COLUMN wh_hi REAL;
ALTER TABLE usage_events ADD COLUMN co2_g_lo REAL;
ALTER TABLE usage_events ADD COLUMN co2_g_hi REAL;
ALTER TABLE usage_events ADD COLUMN method_version TEXT;
CREATE INDEX usage_events_root ON usage_events (root_session_id);
`, `
CREATE TABLE trash_sessions (
	trash_id   TEXT NOT NULL REFERENCES trash(id) ON DELETE CASCADE,
	session_id TEXT NOT NULL,
	root       INTEGER NOT NULL,
	PRIMARY KEY (trash_id, session_id)
);
`, `
ALTER TABLE projects ADD COLUMN last_activity INTEGER NOT NULL DEFAULT 0;
UPDATE projects SET last_activity = created_at;
ALTER TABLE trash ADD COLUMN reason TEXT NOT NULL DEFAULT '';
ALTER TABLE trash ADD COLUMN pending_move INTEGER NOT NULL DEFAULT 0;
`, `
CREATE TABLE IF NOT EXISTS catalog_optins (
	uid  TEXT NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
	item TEXT NOT NULL,
	PRIMARY KEY (uid, item)
);
`, `
CREATE TABLE IF NOT EXISTS connections (
	uid          TEXT NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
	service      TEXT NOT NULL,
	login        TEXT NOT NULL,
	secret_enc   BLOB NOT NULL,
	connected_at INTEGER NOT NULL,
	last_used    INTEGER NOT NULL DEFAULT 0,
	expired      INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (uid, service)
);
CREATE TABLE IF NOT EXISTS onboarding (
	uid     TEXT PRIMARY KEY REFERENCES users(uid) ON DELETE CASCADE,
	seen_at INTEGER NOT NULL
);
`, `
CREATE TABLE IF NOT EXISTS nc_links (
	project_id TEXT PRIMARY KEY,
	uid        TEXT NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
	nc_folder  TEXT NOT NULL,
	alles      INTEGER NOT NULL DEFAULT 0,
	linked_at  INTEGER NOT NULL,
	last_sync  INTEGER NOT NULL DEFAULT 0,
	note       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS nc_files (
	project_id TEXT NOT NULL REFERENCES nc_links(project_id) ON DELETE CASCADE,
	rel        TEXT NOT NULL,
	size       INTEGER NOT NULL,
	mtime      INTEGER NOT NULL,
	etag       TEXT NOT NULL,
	PRIMARY KEY (project_id, rel)
);
CREATE TABLE IF NOT EXISTS nc_collections (
	project_id TEXT NOT NULL,
	uid        TEXT NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
	path       TEXT NOT NULL,
	added_at   INTEGER NOT NULL,
	PRIMARY KEY (project_id, path)
);
`, `
CREATE TABLE IF NOT EXISTS profiles (
	uid        TEXT PRIMARY KEY REFERENCES users(uid) ON DELETE CASCADE,
	facts      TEXT NOT NULL DEFAULT '{}',
	answers    TEXT,
	text       TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL DEFAULT 0
);
`,
	// 9: prototype consent (internal/consent). No FK to users: the router checks it
	// before the account row exists, so a person who declines leaves no account behind.
	`
CREATE TABLE IF NOT EXISTS consents (
	uid         TEXT PRIMARY KEY CHECK (length(uid) = 64),
	version     TEXT NOT NULL,
	accepted_at INTEGER NOT NULL
);
`,
	// 10: chat snapshots for the evaluation (/admin), their AI evaluation and
	// the read audit. No FK to users: DeleteUser keeps an anonymous stub
	// (counts only) under the "deleted-" id ("geloescht-" before the English
	// rename), like usage_events.
	`
CREATE TABLE IF NOT EXISTS chats (
	session_id   TEXT PRIMARY KEY,
	uid          TEXT NOT NULL,
	project      TEXT NOT NULL DEFAULT '',
	title        TEXT NOT NULL DEFAULT '',
	created      INTEGER NOT NULL,
	updated      INTEGER NOT NULL,
	collected_at INTEGER NOT NULL,
	messages     INTEGER NOT NULL DEFAULT 0,
	tool_calls   INTEGER NOT NULL DEFAULT 0,
	tool_errors  INTEGER NOT NULL DEFAULT 0,
	aborted      INTEGER NOT NULL DEFAULT 0,
	model_errors INTEGER NOT NULL DEFAULT 0,
	truncated    INTEGER NOT NULL DEFAULT 0,
	transcript   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS chats_uid ON chats (uid);
CREATE INDEX IF NOT EXISTS chats_updated ON chats (updated);
CREATE TABLE IF NOT EXISTS chat_evals (
	session_id   TEXT PRIMARY KEY REFERENCES chats(session_id) ON DELETE CASCADE,
	chat_updated INTEGER NOT NULL,
	evaluated_at INTEGER NOT NULL,
	source       TEXT NOT NULL,
	result       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS admin_audit (
	ts         INTEGER NOT NULL,
	admin_uid  TEXT NOT NULL,
	admin_name TEXT NOT NULL,
	session_id TEXT NOT NULL,
	action     TEXT NOT NULL
);
`,
	// 11: portal language per person ("" = none chosen: cookie, then the browser).
	// On users, so it goes with the account; before the account the cookie keeps it.
	`
ALTER TABLE users ADD COLUMN lang TEXT NOT NULL DEFAULT '';
`,
	// 12: chats and evaluations keyed by (uid, session_id): a session id is
	// reported by the person's own, untrusted container, so it must never
	// reach another person's snapshot.
	`
CREATE TABLE chats_v12 (
	session_id   TEXT NOT NULL,
	uid          TEXT NOT NULL,
	project      TEXT NOT NULL DEFAULT '',
	title        TEXT NOT NULL DEFAULT '',
	created      INTEGER NOT NULL,
	updated      INTEGER NOT NULL,
	collected_at INTEGER NOT NULL,
	messages     INTEGER NOT NULL DEFAULT 0,
	tool_calls   INTEGER NOT NULL DEFAULT 0,
	tool_errors  INTEGER NOT NULL DEFAULT 0,
	aborted      INTEGER NOT NULL DEFAULT 0,
	model_errors INTEGER NOT NULL DEFAULT 0,
	truncated    INTEGER NOT NULL DEFAULT 0,
	transcript   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (uid, session_id)
);
INSERT INTO chats_v12 SELECT session_id, uid, project, title, created, updated, collected_at, messages, tool_calls,
	tool_errors, aborted, model_errors, truncated, transcript FROM chats;
CREATE TABLE chat_evals_v12 (
	uid          TEXT NOT NULL,
	session_id   TEXT NOT NULL,
	chat_updated INTEGER NOT NULL,
	evaluated_at INTEGER NOT NULL,
	source       TEXT NOT NULL,
	result       TEXT NOT NULL,
	PRIMARY KEY (uid, session_id),
	FOREIGN KEY (uid, session_id) REFERENCES chats_v12 (uid, session_id) ON DELETE CASCADE
);
INSERT INTO chat_evals_v12 SELECT c.uid, e.session_id, e.chat_updated, e.evaluated_at, e.source, e.result
	FROM chat_evals e JOIN chats c ON c.session_id = e.session_id;
DROP TABLE chat_evals;
DROP TABLE chats;
ALTER TABLE chats_v12 RENAME TO chats;
ALTER TABLE chat_evals_v12 RENAME TO chat_evals;
CREATE INDEX chats_updated ON chats (updated);
CREATE INDEX chats_session ON chats (session_id);
`,
	// 13: spend of deleted accounts for the daily cap: DeleteUser moves usage
	// to an anonymous id, so the same uid would start the day at zero again.
	// Rows older than two days are pruned on every deletion.
	`
CREATE TABLE IF NOT EXISTS spend_ledger (
	uid      TEXT NOT NULL,
	ts       INTEGER NOT NULL,
	cost_eur REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS spend_ledger_uid_ts ON spend_ledger (uid, ts);
`,
	// 14: app passwords whose revocation failed (internal/house), retried until
	// Nextcloud confirms. No FK to users: account deletion retries them first.
	`
CREATE TABLE IF NOT EXISTS revocations (
	id         INTEGER PRIMARY KEY,
	uid        TEXT NOT NULL,
	service    TEXT NOT NULL,
	secret_enc BLOB NOT NULL,
	added_at   INTEGER NOT NULL
);
`,
	// 15: who produced the stored Nextcloud version (store.NCFile.Own). Existing
	// rows start as not own: an ETag of unknown origin never authorises an overwrite.
	`
ALTER TABLE nc_files ADD COLUMN own INTEGER NOT NULL DEFAULT 0;
`,
	// 16: when the manager first collected a chat (collected_at is the last
	// collection): ChatByID prefers the earliest, a forged copy comes later.
	`
ALTER TABLE chats ADD COLUMN first_collected INTEGER NOT NULL DEFAULT 0;
UPDATE chats SET first_collected = collected_at;
`,
	// 17: feedback to the operator, from the chat tool feedback_send or the
	// portal form. Context is the manager-rendered tail of the chat. No FK to
	// users: DeleteUser deletes the rows itself.
	`
CREATE TABLE IF NOT EXISTS feedback (
	id         INTEGER PRIMARY KEY,
	ts         INTEGER NOT NULL,
	uid        TEXT NOT NULL,
	source     TEXT NOT NULL,
	art        TEXT NOT NULL,
	text       TEXT NOT NULL,
	project    TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	version    TEXT NOT NULL DEFAULT '',
	context    TEXT NOT NULL DEFAULT '',
	done       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS feedback_uid_ts ON feedback (uid, ts);
`,
	// 18: sessions deleted for good (DeleteTrash), so a feedback stored after
	// the deletion cannot bring the chat excerpt back. Gone with the account.
	`
CREATE TABLE IF NOT EXISTS deleted_sessions (
	uid        TEXT NOT NULL,
	session_id TEXT NOT NULL,
	PRIMARY KEY (uid, session_id)
);
`,
	// 19: files the manager itself put into a person's Nextcloud outside a
	// linked project (ncsync.Upload) with the ETag it got back: only such an
	// own, unchanged file may be replaced (D58). Gone with the account.
	`
CREATE TABLE IF NOT EXISTS nc_ablagen (
	uid  TEXT NOT NULL,
	path TEXT NOT NULL,
	etag TEXT NOT NULL,
	PRIMARY KEY (uid, path)
);
`,
	// 20: English names (docs/RENAME-ENGLISH.md 2f): nc_links.alles ->
	// sync_all, feedback.art -> kind, nc_ablagen -> nc_uploads, plus the stored
	// German ids and JSON keys. All in migrateEnglish, which checks the schema
	// first, so it also runs on a database that already has the new names.
	``,
	// 21: preflight duplicate usernames before changing the schema (migrateIdentity).
	``,
	// 22: staged task-profile pins; preserve restrictions across trash/restore.
	`ALTER TABLE projects ADD COLUMN profile_id TEXT NOT NULL DEFAULT '';
 ALTER TABLE projects ADD COLUMN profile_version INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE projects ADD COLUMN profile_hash TEXT NOT NULL DEFAULT '';
 ALTER TABLE trash ADD COLUMN profile_id TEXT NOT NULL DEFAULT '';
 ALTER TABLE trash ADD COLUMN profile_version INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE trash ADD COLUMN profile_hash TEXT NOT NULL DEFAULT '';`,
	// 23: isolated support chats never share runtime storage or evaluation snapshots.
	`CREATE TABLE profile_projects (
        project_id TEXT PRIMARY KEY, uid TEXT NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
        consent_hash TEXT NOT NULL, evaluation INTEGER NOT NULL DEFAULT 0
    );
    CREATE TABLE profile_sessions (
        id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES profile_projects(project_id) ON DELETE CASCADE,
        uid TEXT NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
        messages TEXT NOT NULL DEFAULT '[]', revision INTEGER NOT NULL DEFAULT 0,
        updated INTEGER NOT NULL, busy_until INTEGER NOT NULL DEFAULT 0
    );`,
	// 24: explicit admission of new profile projects to the ordinary OpenCode runtime.
	`CREATE TABLE opencode_profile_projects (
 project_id TEXT PRIMARY KEY, uid TEXT NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
 profile_hash TEXT NOT NULL
);`,
	`ALTER TABLE projects ADD COLUMN capabilities TEXT NOT NULL DEFAULT 'null';
 ALTER TABLE trash ADD COLUMN capabilities TEXT NOT NULL DEFAULT 'null';`,
	// 26: daily totals filter across people by start time.
	`CREATE INDEX usage_events_ts ON usage_events (ts_start);`,
	// 27: keep upstream billing evidence separate from local EUR estimates.
	`ALTER TABLE usage_events ADD COLUMN raw_cost REAL;
 ALTER TABLE usage_events ADD COLUMN raw_cost_unit TEXT;
 ALTER TABLE usage_events ADD COLUMN raw_cost_source TEXT;`,
}

// migrationFuncs run after the SQL of the migration with that number (1-based),
// in its transaction, for rewrites SQL alone cannot do.
var migrationFuncs = map[int]func(*sql.Tx) error{20: migrateEnglish, 21: migrateIdentity}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serialises all writes; volume is tiny (docs/adr/README.md §3.5).
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the DB is writable: BEGIN IMMEDIATE takes the write lock
// (fails on a read-only file or directory), the rollback writes nothing.
func (s *Store) Ping(ctx context.Context) error {
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "ROLLBACK")
	return err
}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if q := migrations[i]; strings.TrimSpace(q) != "" { // "" = Go-only (migrationFuncs)
			if _, err := tx.Exec(q); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if fn := migrationFuncs[i+1]; fn != nil {
			if err := fn(tx); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

type User struct {
	UID, Username, Email, DisplayName, Slug string
	Slot                                    int
	Generation                              int64
	CreatedAt                               time.Time
}

const userCols = `uid, username, email, display_name, slug, slot, generation, created_at`

type scanner interface{ Scan(...any) error }

func scanUser(r scanner) (User, error) {
	var u User
	var created int64
	err := r.Scan(&u.UID, &u.Username, &u.Email, &u.DisplayName, &u.Slug, &u.Slot, &u.Generation, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	u.CreatedAt = time.UnixMilli(created)
	return u, err
}

func (s *Store) userWhere(ctx context.Context, where string, arg any) (User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE `+where, arg))
}

func (s *Store) UserByUID(ctx context.Context, uid string) (User, error) {
	return s.userWhere(ctx, `uid = ?`, uid)
}
func (s *Store) UserBySlot(ctx context.Context, slot int) (User, error) {
	return s.userWhere(ctx, `slot = ?`, slot)
}

func freeSlug(ctx context.Context, tx *sql.Tx, base string) (string, error) {
	for n := 1; n < 1000; n++ {
		c := identity.Candidate(base, n)
		if c == "" {
			continue
		}
		var x int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE slug = ?`, c).Scan(&x)
		if errors.Is(err, sql.ErrNoRows) {
			return c, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("store: no free slug")
}

func freeSlot(ctx context.Context, tx *sql.Tx) (int, error) {
	var slot int
	// lowest slot >= 1 not in use
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM users WHERE slot = 1)),
		                (SELECT MIN(slot) + 1 FROM users WHERE slot + 1 NOT IN (SELECT slot FROM users)))`).Scan(&slot)
	if err != nil {
		return 0, err
	}
	if slot > config.MaxSlot {
		return 0, errors.New("store: all slots taken")
	}
	return slot, nil
}

// NextGeneration bumps and returns the container generation of uid. It runs
// before every container create so a crash never reuses a password.
func (s *Store) NextGeneration(ctx context.Context, uid string) (int64, error) {
	var g int64
	err := s.db.QueryRowContext(ctx, `UPDATE users SET generation = generation + 1 WHERE uid = ? RETURNING generation`, uid).Scan(&g)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return g, err
}

// UsageEvent is one metered request (docs/adr/README.md §3.7, minimal columns).
// Pointer fields are NULL when the upstream did not report them.
type UsageEvent struct {
	EventID                    string
	Start, End                 time.Time
	UID                        string
	ProjectID, SessionID       string
	RootSessionID              string // chat the call belongs to (subagents included)
	Capability, Target         string
	DeploymentID               string
	Status                     int
	FinishReason               string
	InTokens, CachedTokens     *int64
	OutTokens, Reasoning       *int64
	CostEUR                    *float64
	CostSource                 string
	RawCost                    *float64
	RawCostUnit, RawCostSource string
	WhLo, WhHi                 *float64
	CO2LoG, CO2HiG             *float64
	MethodVersion              string
}

func (s *Store) InsertUsage(ctx context.Context, e UsageEvent) error {
	if e.ProjectID == "" && strings.HasPrefix(e.SessionID, "ses_profile_") {
		// Private session ownership supplies attribution; caller metadata cannot.
		_ = s.db.QueryRowContext(ctx, `SELECT project_id FROM profile_sessions WHERE uid = ? AND id = ?`, e.UID, e.SessionID).Scan(&e.ProjectID)
		e.RootSessionID = e.SessionID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage_events (event_id, ts_start, ts_end, uid, project_id, session_id,
		capability, target, deployment_id, status, finish_reason, in_tokens, cached_tokens, out_tokens, reasoning_tokens,
		cost_eur, cost_source, root_session_id, wh_lo, wh_hi, co2_g_lo, co2_g_hi, method_version, raw_cost, raw_cost_unit, raw_cost_source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.EventID, e.Start.UnixMilli(), e.End.UnixMilli(), e.UID, null(e.ProjectID), null(e.SessionID),
		e.Capability, null(e.Target), null(e.DeploymentID), e.Status, null(e.FinishReason),
		e.InTokens, e.CachedTokens, e.OutTokens, e.Reasoning, e.CostEUR, null(e.CostSource),
		null(e.RootSessionID), e.WhLo, e.WhHi, e.CO2LoG, e.CO2HiG, null(e.MethodVersion), e.RawCost, null(e.RawCostUnit), null(e.RawCostSource))
	return err
}

func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isUnique(err error) bool { return strings.Contains(err.Error(), "UNIQUE constraint failed") }

// UserSlots includes idle users whose network can outlive their container.
func (s *Store) UserSlots(ctx context.Context) ([]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slot FROM users ORDER BY slot`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slots []int
	for rows.Next() {
		var slot int
		if err := rows.Scan(&slot); err != nil {
			return nil, err
		}
		slots = append(slots, slot)
	}
	return slots, rows.Err()
}
