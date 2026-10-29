// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func inviteMember(t *testing.T, s *Store, admin KnowledgeActor, team string, target KnowledgeActor, role string) int {
	t.Helper()
	v, _, _, err := s.Team(context.Background(), admin, team)
	if err != nil {
		t.Fatal(err)
	}
	var stamp string
	if err = s.db.QueryRow(`SELECT incarnation FROM users WHERE uid=?`, target.UID).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamMember(context.Background(), admin, team, target.UID, stamp, role, v.Revision); err != nil {
		t.Fatal(err)
	}
	return v.Revision + 1
}

func TestTeamInvitationMigration(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	team, err := s.CreateTeam(ctx, a[0], "Existing", "")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMember(t, s, a[0], team.ID, a[1], "member")
	if _, err = s.db.Exec(`DROP INDEX team_audit_invites;
 ALTER TABLE team_members DROP COLUMN status;
 ALTER TABLE team_members DROP COLUMN invited_at;
 ALTER TABLE team_members DROP COLUMN accepted_at;
 PRAGMA user_version=30;`); err != nil {
		t.Fatal(err)
	}
	migratedAt := time.Now().UnixMilli()
	if err = s.migrate(); err != nil {
		t.Fatal(err)
	}
	for _, actor := range a[:2] {
		var status string
		var accepted int64
		var noInvitation bool
		if err = s.db.QueryRow(`SELECT status,accepted_at,invited_at IS NULL FROM team_members WHERE team_id=? AND uid=?`, team.ID, actor.UID).Scan(&status, &accepted, &noInvitation); err != nil {
			t.Fatal(err)
		}
		if status != "member" || accepted < migratedAt-1000 || accepted > time.Now().UnixMilli() || !noInvitation {
			t.Fatal(status, accepted, noInvitation)
		}
		if _, _, _, err = s.Team(ctx, actor, team.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTeamInvitationAccessAndAcceptance(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	team, err := s.CreateTeam(ctx, a[0], "Shared team", "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateKnowledgeCollection(ctx, a[0], team.ID, "Shared collection", "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.SaveKnowledgePage(ctx, a[0], testPage(c, "Evidence", "needle"), 0)
	if err != nil {
		t.Fatal(err)
	}
	revision := inviteMember(t, s, a[0], team.ID, a[1], "admin")
	invited, err := s.TeamInvitations(ctx, a[1])
	if err != nil || len(invited) != 1 || invited[0].ID != team.ID {
		t.Fatal(invited, err)
	}
	rows, err := s.Teams(ctx, a[1])
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	_, members, _, err := s.Team(ctx, a[0], team.ID)
	if err != nil || len(members) != 2 {
		t.Fatal(members, err)
	}
	for _, m := range members {
		if m.UID == a[1].UID && (m.Status != "invited" || m.InvitedAt == 0 || m.AcceptedAt != 0) {
			t.Fatal(m)
		}
	}
	if _, _, _, err = s.Team(ctx, a[1], team.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("invited team read", err)
	}
	collections, err := s.KnowledgeCollections(ctx, a[1])
	if err != nil || len(collections) != 0 {
		t.Fatal("invited list", collections, err)
	}
	hits, err := s.SearchKnowledge(ctx, a[1], "", "needle")
	if err != nil || len(hits) != 0 {
		t.Fatal("invited search", hits, err)
	}
	if _, err = s.KnowledgePage(ctx, a[1], c.ID, p.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatal("invited read", err)
	}
	if _, err = s.SaveKnowledgePage(ctx, a[1], p, p.Revision); !errors.Is(err, ErrNotFound) {
		t.Fatal("invited write", err)
	}
	if err = s.SetTeamMember(ctx, a[0], team.ID, a[0].UID, "", "", revision); !errors.Is(err, ErrLastTeamAdmin) {
		t.Fatal("pending admin counted", err)
	}
	project := Project{ID: uuid.NewString(), UID: a[1].UID, Folder: "sources", CreatedAt: time.Now()}
	if err = s.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err = s.BindKnowledge(ctx, a[1], project.ID, c.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatal("invited binding", err)
	}
	// A retained binding from an earlier membership cannot bypass the current status.
	if _, err = s.db.Exec(`INSERT INTO project_knowledge VALUES(?,?)`, project.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	bound, err := s.ProjectKnowledge(ctx, a[1], project.ID)
	if err != nil || len(bound) != 0 {
		t.Fatal("invited project list", bound, err)
	}
	if err = s.RespondTeamInvitation(ctx, a[2], team.ID, true, revision); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign acceptance", err)
	}
	if err = s.RespondTeamInvitation(ctx, a[1], team.ID, true, revision-1); !errors.Is(err, ErrKnowledgeConflict) {
		t.Fatal("stale acceptance", err)
	}
	if err = s.RespondTeamInvitation(ctx, a[1], team.ID, true, revision); err != nil {
		t.Fatal(err)
	}
	if err = s.RespondTeamInvitation(ctx, a[1], team.ID, true, revision); !errors.Is(err, ErrNotFound) {
		t.Fatal("repeated acceptance", err)
	}
	if _, err = s.KnowledgePage(ctx, a[1], c.ID, p.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.BindKnowledge(ctx, a[1], project.ID, c.ID, true); err != nil {
		t.Fatal(err)
	}
	accepted, members, audit, err := s.Team(ctx, a[1], team.ID)
	if err != nil || accepted.Admins != 2 || audit[0].Action != "accept" || audit[1].Action != "invite" {
		t.Fatal(accepted, audit, err)
	}
	for _, m := range members {
		if m.Status != "member" || m.AcceptedAt == 0 {
			t.Fatal(m)
		}
	}
	if err = s.SetTeamMember(ctx, a[1], team.ID, a[1].UID, "", "", accepted.Revision); err != nil {
		t.Fatal("leave", err)
	}
}

func TestTeamInvitationDeclineWithdrawAndRecovery(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	team, err := s.CreateTeam(ctx, a[0], "Team", "")
	if err != nil {
		t.Fatal(err)
	}
	revision := inviteMember(t, s, a[0], team.ID, a[1], "member")
	if err = s.RespondTeamInvitation(ctx, a[1], team.ID, false, revision); err != nil {
		t.Fatal(err)
	}
	invitations, err := s.TeamInvitations(ctx, a[1])
	if err != nil || len(invitations) != 0 {
		t.Fatal(invitations, err)
	}
	revision = inviteMember(t, s, a[0], team.ID, a[1], "member")
	if err = s.SetTeamMember(ctx, a[0], team.ID, a[1].UID, "", "", revision); err != nil {
		t.Fatal(err)
	}
	if err = s.RespondTeamInvitation(ctx, a[1], team.ID, true, revision); !errors.Is(err, ErrNotFound) {
		t.Fatal("withdrawn acceptance", err)
	}
	_, _, audit, err := s.Team(ctx, a[0], team.ID)
	if err != nil || audit[0].Action != "withdraw" || audit[2].Action != "decline" {
		t.Fatal(audit, err)
	}
	// Recovery must invite outsiders too, and an orphan's invited admin can accept.
	if _, err = s.DeleteUser(ctx, a[0].UID, "deleted"); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverTeam(ctx, a[2], team.ID, "person2", revision+1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = s.Team(ctx, a[1], team.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err = s.RespondTeamInvitation(ctx, a[1], team.ID, true, revision+2); err != nil {
		t.Fatal(err)
	}
}

func TestTeamMembershipCap(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	for i := range 20 {
		team, err := s.CreateTeam(ctx, a[0], fmt.Sprintf("Team %d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		revision := inviteMember(t, s, a[0], team.ID, a[1], "member")
		if i%2 == 0 {
			if err = s.RespondTeamInvitation(ctx, a[1], team.ID, true, revision); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, actor := range a[:2] {
		if _, err := s.CreateTeam(ctx, actor, "Over limit", ""); !errors.Is(err, ErrTeamMembershipLimit) {
			t.Fatal(err)
		}
	}
	team, err := s.CreateTeam(ctx, a[2], "Another", "")
	if err != nil {
		t.Fatal(err)
	}
	var stamp string
	if err = s.db.QueryRow(`SELECT incarnation FROM users WHERE uid=?`, a[1].UID).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamMember(ctx, a[2], team.ID, a[1].UID, stamp, "member", 1); !errors.Is(err, ErrTeamMembershipLimit) {
		t.Fatal(err)
	}
	invitations, err := s.TeamInvitations(ctx, a[1])
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RespondTeamInvitation(ctx, a[1], invitations[0].ID, false, invitations[0].Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamMember(ctx, a[2], team.ID, a[1].UID, stamp, "member", 1); err != nil {
		t.Fatal(err)
	}
}

func TestTeamInvitationDailyCap(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	team, err := s.CreateTeam(ctx, a[0], "Team", "")
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		revision := inviteMember(t, s, a[0], team.ID, a[1], "member")
		if err = s.RespondTeamInvitation(ctx, a[1], team.ID, false, revision); err != nil {
			t.Fatal(err)
		}
	}
	var stamp string
	if err = s.db.QueryRow(`SELECT incarnation FROM users WHERE uid=?`, a[1].UID).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamMember(ctx, a[0], team.ID, a[1].UID, stamp, "member", 101); !errors.Is(err, ErrTeamInviteLimit) {
		t.Fatal(err)
	}
	// Invitations from yesterday do not consume today's budget.
	if _, err = s.db.Exec(`UPDATE team_audit SET created=? WHERE action='invite'`, time.Now().UTC().Add(-24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTeamMember(ctx, a[0], team.ID, a[1].UID, stamp, "member", 101); err != nil {
		t.Fatal(err)
	}
}

func TestTeamCandidateBoundsAndRateLimit(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	team, err := s.CreateTeam(ctx, a[0], "Team", "")
	if err != nil {
		t.Fatal(err)
	}
	addTeamMember(t, s, a[0], team.ID, a[1], "admin")
	for i := 10; i < 22; i++ {
		if _, err = s.EnsureUser(ctx, identity.Identity{UID: uid(i), Username: fmt.Sprintf("candidate%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	for range 35 {
		rows, err := s.TeamCandidates(ctx, a[0], team.ID, " ca ")
		if err != nil || len(rows) != 0 {
			t.Fatal(rows, err)
		}
	}
	for range 30 {
		rows, err := s.TeamCandidates(ctx, a[0], team.ID, " can ")
		if err != nil || len(rows) != 10 {
			t.Fatal(rows, err)
		}
	}
	other, err := s.CreateTeam(ctx, a[0], "Second team", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TeamCandidates(ctx, a[0], other.ID, "candidate"); !errors.Is(err, ErrTeamSearchLimit) {
		t.Fatal("uid limit", err)
	}
	if _, err = s.TeamCandidates(ctx, a[1], team.ID, "candidate"); err != nil {
		t.Fatal("independent uid", err)
	}
}
