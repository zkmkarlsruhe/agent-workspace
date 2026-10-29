// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
)

var (
	ErrKnowledgeConflict   = errors.New("This item changed. Review the current version before saving again.")
	ErrKnowledgeInvalid    = errors.New("Check the title, content, selection and size limits.")
	ErrKnowledgeReadOnly   = errors.New("This item is read-only. Restore it or assign an active team administrator first.")
	ErrTeamMembershipLimit = errors.New("A person can have at most 20 memberships and pending invitations. Leave a team or decline an invitation first.")
	ErrTeamInviteLimit     = errors.New("This team has reached its limit of 50 invitations per day. Try again tomorrow.")
	ErrTeamSearchLimit     = errors.New("Account search is limited to 30 requests per minute. Wait before searching again.")
	ErrLastTeamAdmin       = errors.New("Assign another active team administrator before leaving or removing this role.")
)

// KnowledgeActor is captured from the authenticated account, never from request arguments.
// Forms retain Stamp so a stale submit cannot cross account deletion or identity rebinding.
type KnowledgeActor struct{ UID, Stamp, ProjectID string }

const knowledgeAccount = `SELECT json_array(incarnation, identity_issuer, identity_subject)
 FROM users u JOIN consents c ON c.uid=u.uid
 WHERE u.uid=? AND u.blocked_subject IS NULL AND c.version=?`

func (s *Store) KnowledgeActor(ctx context.Context, uid string) (KnowledgeActor, error) {
	a := KnowledgeActor{UID: uid}
	err := s.db.QueryRowContext(ctx, knowledgeAccount, uid, consent.Version()).Scan(&a.Stamp)
	return a, knowledgeError(err)
}

func knowledgeError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) knowledgeTx(ctx context.Context, a KnowledgeActor, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var stamp string
	if err := tx.QueryRowContext(ctx, knowledgeAccount, a.UID, consent.Version()).Scan(&stamp); err != nil {
		return knowledgeError(err)
	}
	if stamp != a.Stamp || stamp == "" {
		return ErrNotFound
	}
	if a.ProjectID != "" {
		var owned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=? AND uid=? AND profile_id='')`, a.ProjectID, a.UID).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return ErrNotFound
		}
	}
	if err := fn(tx); err != nil {
		return knowledgeError(err)
	}
	return tx.Commit()
}

func validKnowledgeTitle(title string) bool {
	return strings.TrimSpace(title) != "" && utf8.ValidString(title) && utf8.RuneCountInString(title) <= 100 && !strings.ContainsAny(title, "\x00\r\n")
}

const activeTeamAdmins = `(SELECT COUNT(*) FROM team_members am JOIN users au ON au.uid=am.uid
 WHERE am.team_id=t.id AND am.role='admin' AND am.status='member' AND au.blocked_subject IS NULL)`

// All team and collection access queries use this predicate with alias m.
const acceptedTeamMember = `m.status='member'`
const teamColumns = `t.id,t.name,t.description,t.revision,t.archived,t.created,` + activeTeamAdmins

type Team struct {
	ID, Name, Description, Role string
	Revision                    int
	Archived                    bool
	Created                     int64
	Admins                      int
}

func (t Team) Writable() bool { return !t.Archived && t.Admins > 0 }

type TeamMember struct {
	UID, Name, Role, Status string
	InvitedAt, AcceptedAt   int64
	Blocked                 bool
}
type TeamCandidate struct{ UID, Name, Stamp string }
type TeamAudit struct {
	Actor, Subject, Action string
	Created                int64
}

func scanTeam(row scanner) (t Team, err error) {
	err = row.Scan(&t.ID, &t.Name, &t.Description, &t.Revision, &t.Archived, &t.Created, &t.Admins, &t.Role)
	return t, knowledgeError(err)
}
func teamFor(ctx context.Context, tx *sql.Tx, a KnowledgeActor, id string) (Team, error) {
	return scanTeam(tx.QueryRowContext(ctx, `SELECT `+teamColumns+`,m.role FROM teams t JOIN team_members m ON m.team_id=t.id WHERE t.id=? AND m.uid=? AND `+acceptedTeamMember, id, a.UID))
}
func auditTeam(ctx context.Context, tx *sql.Tx, a KnowledgeActor, team, subject, action string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO team_audit(team_id,actor,subject,action,created) VALUES(?,?,NULLIF(?,''),?,?)`, team, a.UID, subject, action, time.Now().UnixMilli())
	return err
}

func (s *Store) Teams(ctx context.Context, a KnowledgeActor) (out []Team, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, `SELECT `+teamColumns+`,m.role FROM teams t JOIN team_members m ON m.team_id=t.id WHERE m.uid=? AND `+acceptedTeamMember+` ORDER BY t.name,t.id`, a.UID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			t, e := scanTeam(rows)
			if e != nil {
				return e
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return
}
func (s *Store) CreateTeam(ctx context.Context, a KnowledgeActor, name, description string) (t Team, err error) {
	if !validKnowledgeTitle(name) || len(description) > 4000 {
		return t, ErrKnowledgeInvalid
	}
	t = Team{ID: uuid.NewString(), Name: strings.TrimSpace(name), Description: description, Role: "admin", Revision: 1, Admins: 1, Created: time.Now().UnixMilli()}
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		if e := teamMembershipCapacity(ctx, tx, a.UID); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO teams(id,name,description,creator,created) VALUES(?,?,?,?,?)`, t.ID, t.Name, t.Description, a.UID, t.Created); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO team_members(team_id,uid,role,status,accepted_at) VALUES(?,?,'admin','member',?)`, t.ID, a.UID, t.Created); e != nil {
			return e
		}
		return auditTeam(ctx, tx, a, t.ID, a.UID, "create")
	})
	return
}
func (s *Store) Team(ctx context.Context, a KnowledgeActor, id string) (t Team, members []TeamMember, audit []TeamAudit, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		var e error
		t, e = teamFor(ctx, tx, a, id)
		if e != nil {
			return e
		}
		rows, e := tx.QueryContext(ctx, `SELECT u.uid,COALESCE(NULLIF(u.display_name,''),u.username),m.role,m.status,COALESCE(m.invited_at,0),COALESCE(m.accepted_at,0),u.blocked_subject IS NOT NULL FROM team_members m JOIN users u ON u.uid=m.uid WHERE m.team_id=? AND (`+acceptedTeamMember+` OR ?='admin') ORDER BY m.status,m.role,u.username`, id, t.Role)
		if e != nil {
			return e
		}
		for rows.Next() {
			var m TeamMember
			if e = rows.Scan(&m.UID, &m.Name, &m.Role, &m.Status, &m.InvitedAt, &m.AcceptedAt, &m.Blocked); e != nil {
				rows.Close()
				return e
			}
			members = append(members, m)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if t.Role != "admin" {
			return nil
		}
		rows, e = tx.QueryContext(ctx, `SELECT COALESCE(NULLIF(a.display_name,''),a.username,'Former member'),COALESCE(NULLIF(u.display_name,''),u.username,'Former member'),v.action,v.created FROM team_audit v LEFT JOIN users a ON a.uid=v.actor LEFT JOIN users u ON u.uid=v.subject WHERE v.team_id=? ORDER BY v.id DESC LIMIT 100`, id)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var v TeamAudit
			if e = rows.Scan(&v.Actor, &v.Subject, &v.Action, &v.Created); e != nil {
				return e
			}
			audit = append(audit, v)
		}
		return rows.Err()
	})
	return
}

// TeamCandidates is a bounded search available only to admins of this team.
func (s *Store) TeamCandidates(ctx context.Context, a KnowledgeActor, id, query string) (out []TeamCandidate, err error) {
	query = strings.TrimSpace(query)
	if utf8.RuneCountInString(query) < 3 || len(query) > 100 {
		return out, nil
	}
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		t, e := teamFor(ctx, tx, a, id)
		if e != nil {
			return e
		}
		if t.Role != "admin" {
			return ErrNotFound
		}
		if !s.teamSearch.Allow(a.UID, 30, time.Minute, time.Now()) {
			return ErrTeamSearchLimit
		}
		rows, e := tx.QueryContext(ctx, `SELECT uid,COALESCE(NULLIF(display_name,''),username),incarnation FROM users WHERE blocked_subject IS NULL AND (instr(lower(username),lower(?))>0 OR instr(lower(display_name),lower(?))>0) AND NOT EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=? AND m.uid=users.uid) ORDER BY username LIMIT 10`, query, query, id)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var c TeamCandidate
			if e = rows.Scan(&c.UID, &c.Name, &c.Stamp); e != nil {
				return e
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return
}

func (s *Store) SetTeamMember(ctx context.Context, a KnowledgeActor, id, target, targetStamp, role string, expected int) error {
	if role != "" && role != "admin" && role != "member" {
		return ErrKnowledgeInvalid
	}
	return s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		t, e := teamFor(ctx, tx, a, id)
		if e != nil {
			return e
		}
		if t.Role != "admin" && !(a.UID == target && role == "") {
			return ErrNotFound
		}
		if t.Revision != expected {
			return ErrKnowledgeConflict
		}
		if !t.Writable() && role != "" {
			return ErrKnowledgeReadOnly
		}
		var old, status string
		e = tx.QueryRowContext(ctx, `SELECT role,status FROM team_members WHERE team_id=? AND uid=?`, id, target).Scan(&old, &status)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if old == "admin" && status == "member" && role != "admin" {
			var active bool
			if e := tx.QueryRowContext(ctx, `SELECT blocked_subject IS NULL FROM users WHERE uid=?`, target).Scan(&active); e != nil {
				return e
			}
			if active && t.Admins <= 1 {
				return ErrLastTeamAdmin
			}
		}
		if role == "" {
			if old == "" {
				return ErrNotFound
			}
			action := "remove"
			if status == "invited" {
				action = "withdraw"
			}
			if e := auditTeam(ctx, tx, a, id, target, action); e != nil {
				return e
			}
			if _, e := tx.ExecContext(ctx, `DELETE FROM team_members WHERE team_id=? AND uid=?`, id, target); e != nil {
				return e
			}
		} else {
			var stamp string
			if e := tx.QueryRowContext(ctx, `SELECT incarnation FROM users WHERE uid=? AND blocked_subject IS NULL`, target).Scan(&stamp); e != nil {
				return e
			}
			// A new member is selected from a specific existing account, not a reusable name.
			if old == "" && (targetStamp == "" || targetStamp != stamp) {
				return ErrNotFound
			}
			if old == "" {
				if e := inviteTeamMember(ctx, tx, a, id, target, role); e != nil {
					return e
				}
			} else {
				if _, e := tx.ExecContext(ctx, `UPDATE team_members SET role=? WHERE team_id=? AND uid=?`, role, id, target); e != nil {
					return e
				}
				if e := auditTeam(ctx, tx, a, id, target, "role:"+role); e != nil {
					return e
				}
			}
		}
		_, e = tx.ExecContext(ctx, `UPDATE teams SET revision=revision+1 WHERE id=?`, id)
		return e
	})
}
func (s *Store) UpdateTeam(ctx context.Context, a KnowledgeActor, id, name, description string, archived bool, expected int) error {
	if !validKnowledgeTitle(name) || len(description) > 4000 {
		return ErrKnowledgeInvalid
	}
	return s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		t, e := teamFor(ctx, tx, a, id)
		if e != nil {
			return e
		}
		if t.Role != "admin" {
			return ErrNotFound
		}
		if t.Revision != expected {
			return ErrKnowledgeConflict
		}
		if t.Admins == 0 {
			return ErrKnowledgeReadOnly
		}
		if _, e := tx.ExecContext(ctx, `UPDATE teams SET name=?,description=?,archived=?,revision=revision+1 WHERE id=?`, strings.TrimSpace(name), description, archived, id); e != nil {
			return e
		}
		return auditTeam(ctx, tx, a, id, "", "settings")
	})
}

// Recovery is called only by the separately authorized instance-admin portal route.
// It exposes team metadata only and cannot read personal knowledge or conversations.
func (s *Store) OrphanTeams(ctx context.Context, a KnowledgeActor) (out []Team, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, `SELECT `+teamColumns+`,'' FROM teams t WHERE `+activeTeamAdmins+`=0 ORDER BY t.created LIMIT 100`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			t, e := scanTeam(rows)
			if e != nil {
				return e
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return
}
func (s *Store) RecoverTeam(ctx context.Context, a KnowledgeActor, id, username string, expected int) error {
	return s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		t, e := scanTeam(tx.QueryRowContext(ctx, `SELECT `+teamColumns+`,'' FROM teams t WHERE id=?`, id))
		if e != nil {
			return e
		}
		if t.Revision != expected || t.Admins != 0 {
			return ErrKnowledgeConflict
		}
		var target string
		if e := tx.QueryRowContext(ctx, `SELECT uid FROM users WHERE username=? AND blocked_subject IS NULL`, username).Scan(&target); e != nil {
			return e
		}
		var exists bool
		if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_members WHERE team_id=? AND uid=?)`, id, target).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			if e := inviteTeamMember(ctx, tx, a, id, target, "admin"); e != nil {
				return e
			}
		} else if _, e := tx.ExecContext(ctx, `UPDATE team_members SET role='admin' WHERE team_id=? AND uid=?`, id, target); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `UPDATE teams SET revision=revision+1 WHERE id=?`, id); e != nil {
			return e
		}
		return auditTeam(ctx, tx, a, id, target, "recover")
	})
}

func (s *Store) PurgeTeam(ctx context.Context, a KnowledgeActor, id string, expected int) error {
	return s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		t, err := teamFor(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if t.Role != "admin" {
			return ErrNotFound
		}
		if t.Revision != expected {
			return ErrKnowledgeConflict
		}
		if !t.Archived {
			return ErrKnowledgeReadOnly
		}
		var owned int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_collections WHERE team_id=?`, id).Scan(&owned); err != nil {
			return err
		}
		if owned != 0 {
			return ErrKnowledgeInvalid
		}
		if err := auditTeam(ctx, tx, a, id, "", "delete"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM teams WHERE id=?`, id)
		return err
	})
}

func teamMembershipCapacity(ctx context.Context, tx *sql.Tx, uid string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM team_members WHERE uid=?`, uid).Scan(&n); err != nil {
		return err
	}
	if n >= 20 {
		return ErrTeamMembershipLimit
	}
	return nil
}

func inviteTeamMember(ctx context.Context, tx *sql.Tx, a KnowledgeActor, team, target, role string) error {
	if err := teamMembershipCapacity(ctx, tx, target); err != nil {
		return err
	}
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM team_audit WHERE team_id=? AND action='invite' AND created>=?`, team, day).Scan(&n); err != nil {
		return err
	}
	if n >= 50 {
		return ErrTeamInviteLimit
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO team_members(team_id,uid,role,status,invited_at) VALUES(?,?,?,'invited',?)`, team, target, role, now.UnixMilli()); err != nil {
		return err
	}
	return auditTeam(ctx, tx, a, team, target, "invite")
}

// TeamInvitations exposes only the metadata needed to decide whether to join.
func (s *Store) TeamInvitations(ctx context.Context, a KnowledgeActor) (out []Team, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, `SELECT `+teamColumns+`,m.role FROM teams t JOIN team_members m ON m.team_id=t.id WHERE m.uid=? AND m.status='invited' ORDER BY m.invited_at,t.id`, a.UID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			t, e := scanTeam(rows)
			if e != nil {
				return e
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return
}

func (s *Store) RespondTeamInvitation(ctx context.Context, a KnowledgeActor, id string, accept bool, expected int) error {
	return s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		t, err := scanTeam(tx.QueryRowContext(ctx, `SELECT `+teamColumns+`,m.role FROM teams t JOIN team_members m ON m.team_id=t.id WHERE t.id=? AND m.uid=? AND m.status='invited'`, id, a.UID))
		if err != nil {
			return err
		}
		if t.Revision != expected {
			return ErrKnowledgeConflict
		}
		action := "decline"
		if accept {
			// Recovery may invite an administrator while the team has no active admin.
			if t.Archived || (t.Admins == 0 && t.Role != "admin") {
				return ErrKnowledgeReadOnly
			}
			_, err = tx.ExecContext(ctx, `UPDATE team_members SET status='member',accepted_at=? WHERE team_id=? AND uid=?`, time.Now().UnixMilli(), id, a.UID)
			action = "accept"
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM team_members WHERE team_id=? AND uid=?`, id, a.UID)
		}
		if err != nil {
			return err
		}
		if err = auditTeam(ctx, tx, a, id, a.UID, action); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE teams SET revision=revision+1 WHERE id=?`, id)
		return err
	})
}
