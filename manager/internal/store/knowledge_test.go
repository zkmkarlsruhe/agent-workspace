// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func knowledgeSetup(t *testing.T) (*Store, []KnowledgeActor) {
	t.Helper()
	s := open(t)
	var actors []KnowledgeActor
	ctx := context.Background()
	for i := 1; i <= 4; i++ {
		u, e := s.EnsureUser(ctx, identity.Identity{UID: uid(i), Username: fmt.Sprintf("person%d", i)})
		if e != nil {
			t.Fatal(e)
		}
		if e = s.AcceptConsent(ctx, u.UID, consent.Version(), time.Now()); e != nil {
			t.Fatal(e)
		}
		a, e := s.KnowledgeActor(ctx, u.UID)
		if e != nil {
			t.Fatal(e)
		}
		actors = append(actors, a)
	}
	return s, actors
}
func addTeamMember(t *testing.T, s *Store, a KnowledgeActor, team string, who KnowledgeActor, role string) {
	t.Helper()
	ctx := context.Background()
	v, _, _, e := s.Team(ctx, a, team)
	if e != nil {
		t.Fatal(e)
	}
	var stamp string
	if e = s.db.QueryRow(`SELECT incarnation FROM users WHERE uid=?`, who.UID).Scan(&stamp); e != nil {
		t.Fatal(e)
	}
	if e = s.SetTeamMember(ctx, a, team, who.UID, stamp, role, v.Revision); e != nil {
		t.Fatal(e)
	}
	if e = s.RespondTeamInvitation(ctx, who, team, true, v.Revision+1); e != nil {
		t.Fatal(e)
	}
}
func testPage(c KnowledgeCollection, title, body string) KnowledgePage {
	return KnowledgePage{ID: uuid.NewString(), CollectionID: c.ID, Title: title, Body: body, Sources: "https://example.org/source, captured 2026-10-29"}
}

func TestKnowledgeMigrationPreservesAccountsAndAssistantConfig(t *testing.T) {
	s, actors := knowledgeSetup(t)
	if _, err := s.db.Exec(`INSERT INTO assistant_config VALUES(1, '[]');
 DROP TRIGGER users_incarnation; ALTER TABLE users DROP COLUMN incarnation;
 DROP TABLE project_knowledge; DROP TABLE knowledge_revisions; DROP TABLE knowledge_pages;
 DROP TABLE knowledge_collections; DROP TABLE knowledge_search;
 DROP TABLE team_audit; DROP TABLE team_members; DROP TABLE teams;
 PRAGMA user_version = 29;`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	var definitions string
	if err := s.db.QueryRow(`SELECT definitions FROM assistant_config WHERE id=1`).Scan(&definitions); err != nil || definitions != "[]" {
		t.Fatal("assistant configuration changed", definitions, err)
	}
	seen := map[string]bool{}
	for _, actor := range actors {
		var incarnation string
		if err := s.db.QueryRow(`SELECT incarnation FROM users WHERE uid=?`, actor.UID).Scan(&incarnation); err != nil || len(incarnation) != 32 || seen[incarnation] {
			t.Fatal("missing or reused account incarnation", incarnation, err)
		}
		seen[incarnation] = true
		fresh, err := s.KnowledgeActor(context.Background(), actor.UID)
		if err != nil || fresh.Stamp == actor.Stamp {
			t.Fatal("account consent lost or stale identity retained", fresh, err)
		}
		if _, err = s.CreateKnowledgeCollection(context.Background(), fresh, "", "Migrated account", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKnowledgeOwnershipRevisionsSearchAndRevocation(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	team, e := s.CreateTeam(ctx, a[0], "Exhibition", "Cross-department project")
	if e != nil {
		t.Fatal(e)
	}
	if team.Role != "admin" || team.Admins != 1 {
		t.Fatal(team)
	}
	addTeamMember(t, s, a[0], team.ID, a[1], "member")
	private, e := s.CreateKnowledgeCollection(ctx, a[0], "", "Private", "private metadata")
	if e != nil {
		t.Fatal(e)
	}
	shared, e := s.CreateKnowledgeCollection(ctx, a[0], team.ID, "Shared", "team metadata")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateKnowledgeCollection(ctx, a[1], team.ID, "Unauthorized", ""); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	secret := testPage(private, "Private title", "secret störung")
	secret, e = s.SaveKnowledgePage(ctx, a[0], secret, 0)
	if e != nil {
		t.Fatal(e)
	}
	p := testPage(shared, "Störung projector", "Die Störung betrifft den Beamer. Sources remain unchanged.")
	p, e = s.SaveKnowledgePage(ctx, a[1], p, 0)
	if e != nil {
		t.Fatal(e)
	}
	for _, actor := range a[1:] {
		if _, e = s.KnowledgePage(ctx, actor, private.ID, secret.ID, 1); !errors.Is(e, ErrNotFound) {
			t.Fatal("private read", e)
		}
		if _, e = s.KnowledgeHistory(ctx, actor, private.ID, secret.ID); !errors.Is(e, ErrNotFound) {
			t.Fatal("private history", e)
		}
		if _, e = s.ExportKnowledge(ctx, actor, private.ID); !errors.Is(e, ErrNotFound) {
			t.Fatal("private export", e)
		}
	}
	hits, e := s.SearchKnowledge(ctx, a[1], "", "störung")
	if e != nil || len(hits) != 1 || hits[0].PageID != p.ID {
		t.Fatal(hits, e)
	}
	hits, e = s.SearchKnowledge(ctx, a[2], "", "störung")
	if e != nil || len(hits) != 0 {
		t.Fatal("foreign search", hits, e)
	}
	if _, e = s.ExportKnowledge(ctx, a[1], shared.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal("member bulk export", e)
	}
	updated := p
	updated.Body = "new text"
	updated, e = s.SaveKnowledgePage(ctx, a[0], updated, 1)
	if e != nil {
		t.Fatal(e)
	}
	p.Body = "stale text"
	if _, e = s.SaveKnowledgePage(ctx, a[1], p, 1); !errors.Is(e, ErrKnowledgeConflict) {
		t.Fatal(e)
	}
	old, e := s.KnowledgePage(ctx, a[1], shared.ID, p.ID, 1)
	if e != nil || old.Body == "new text" || old.Sources != p.Sources {
		t.Fatal(old, e)
	}
	restored, e := s.RestoreKnowledgeRevision(ctx, a[0], shared.ID, p.ID, 1, updated.Revision)
	if e != nil || restored.Revision != 3 || restored.Body != old.Body {
		t.Fatal(restored, e)
	}
	tr, _, _, _ := s.Team(ctx, a[0], team.ID)
	if e = s.SetTeamMember(ctx, a[0], team.ID, a[1].UID, "", "", tr.Revision); e != nil {
		t.Fatal(e)
	}
	if _, e = s.KnowledgePage(ctx, a[1], shared.ID, p.ID, 0); !errors.Is(e, ErrNotFound) {
		t.Fatal("revoked read", e)
	}
	if _, e = s.SaveKnowledgePage(ctx, a[1], restored, 3); !errors.Is(e, ErrNotFound) {
		t.Fatal("revoked write", e)
	}
	hits, e = s.SearchKnowledge(ctx, a[1], "", "störung")
	if e != nil || len(hits) != 0 {
		t.Fatal("revoked search", hits, e)
	}
}

func TestTeamLastAdminRaceAndFounderDeletion(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	team, e := s.CreateTeam(ctx, a[0], "Team", "")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SetTeamMember(ctx, a[0], team.ID, a[0].UID, "", "", 1); !errors.Is(e, ErrLastTeamAdmin) {
		t.Fatal(e)
	}
	addTeamMember(t, s, a[0], team.ID, a[1], "admin")
	addTeamMember(t, s, a[0], team.ID, a[2], "member")
	c, e := s.CreateKnowledgeCollection(ctx, a[0], team.ID, "Shared", "")
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.SaveKnowledgePage(ctx, a[0], testPage(c, "Survives", "team evidence"), 0)
	if e != nil {
		t.Fatal(e)
	}
	v, _, _, _ := s.Team(ctx, a[0], team.ID)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, actor := range a[:2] {
		wg.Go(func() { errs <- s.SetTeamMember(ctx, actor, team.ID, actor.UID, "", "member", v.Revision) })
	}
	wg.Wait()
	close(errs)
	successes := 0
	for e := range errs {
		if e == nil {
			successes++
		} else if !errors.Is(e, ErrKnowledgeConflict) {
			t.Fatal(e)
		}
	}
	if successes != 1 {
		t.Fatal(successes)
	}
	if _, e = s.DeleteUser(ctx, a[0].UID, "deleted-one"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DeleteUser(ctx, a[1].UID, "deleted-two"); e != nil {
		t.Fatal(e)
	}
	v, _, _, e = s.Team(ctx, a[2], team.ID)
	if e != nil || v.Admins != 0 || v.Writable() {
		t.Fatal(v, e)
	}
	retained, e := s.KnowledgePage(ctx, a[2], c.ID, p.ID, 0)
	if e != nil || retained.Body != p.Body || retained.Actor != "" {
		t.Fatal(retained, e)
	}
	if _, e = s.SaveKnowledgePage(ctx, a[2], retained, 1); !errors.Is(e, ErrKnowledgeReadOnly) {
		t.Fatal(e)
	}
	if e = s.RecoverTeam(ctx, a[3], team.ID, "person3", v.Revision); e != nil {
		t.Fatal(e)
	}
	v, _, audit, e := s.Team(ctx, a[2], team.ID)
	if e != nil || v.Role != "admin" || len(audit) == 0 || audit[0].Action != "recover" {
		t.Fatal(v, audit, e)
	}
}

func TestKnowledgeBlockedRecreatedAndPurgedAccounts(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	c, e := s.CreateKnowledgeCollection(ctx, a[0], "", "Private", "")
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.SaveKnowledgePage(ctx, a[0], testPage(c, "Private", "needle"), 0)
	if e != nil {
		t.Fatal(e)
	}
	team, e := s.CreateTeam(ctx, a[0], "Team", "")
	if e != nil {
		t.Fatal(e)
	}
	addTeamMember(t, s, a[0], team.ID, a[1], "member")
	if _, e = s.db.Exec(`UPDATE users SET blocked_issuer='test',blocked_subject='other' WHERE uid=?`, a[0].UID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.KnowledgePage(ctx, a[0], c.ID, p.ID, 0); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	v, _, _, e := s.Team(ctx, a[1], team.ID)
	if e != nil || v.Admins != 0 {
		t.Fatal(v, e)
	}
	if _, e = s.DeleteUser(ctx, a[0].UID, "deleted"); e != nil {
		t.Fatal(e)
	}
	u, e := s.EnsureUser(ctx, identity.Identity{UID: a[0].UID, Username: "person1"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.AcceptConsent(ctx, u.UID, consent.Version(), time.Now()); e != nil {
		t.Fatal(e)
	}
	fresh, e := s.KnowledgeActor(ctx, u.UID)
	if e != nil || fresh.Stamp == a[0].Stamp {
		t.Fatal(fresh, e)
	}
	if _, e = s.CreateTeam(ctx, a[0], "Stale actor", ""); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	if _, e = s.KnowledgePage(ctx, fresh, c.ID, p.ID, 0); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	var n int
	if e = s.db.QueryRow(`SELECT COUNT(*) FROM knowledge_search WHERE page_id=?`, p.ID).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}

func TestKnowledgeHierarchyExportImportAndIdempotency(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	c, e := s.CreateKnowledgeCollection(ctx, a[0], "", "Notes", "")
	if e != nil {
		t.Fatal(e)
	}
	p := testPage(c, "Parent", "parent")
	p, e = s.SaveKnowledgePage(ctx, a[0], p, 0)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := s.SaveKnowledgePage(ctx, a[0], p, 0)
	if e != nil || replay.Revision != 1 {
		t.Fatal(replay, e)
	}
	child := testPage(c, "Child", "child")
	child.Parent = p.ID
	child, e = s.SaveKnowledgePage(ctx, a[0], child, 0)
	if e != nil {
		t.Fatal(e)
	}
	p.Parent = child.ID
	if _, e = s.SaveKnowledgePage(ctx, a[0], p, 1); !errors.Is(e, ErrKnowledgeInvalid) {
		t.Fatal("cycle", e)
	}
	p.Parent = ""
	b, e := s.ExportKnowledge(ctx, a[0], c.ID)
	if e != nil || len(b.Pages) != 2 {
		t.Fatal(b, e)
	}
	for _, x := range b.Pages {
		if x.Actor != "" || x.CollectionID != "" {
			t.Fatal("identity leaked", x)
		}
	}
	if data, e := b.ZIP(); e != nil || len(data) == 0 {
		t.Fatal(e)
	}
	imported, e := s.ImportKnowledge(ctx, a[1], "", b)
	if e != nil {
		t.Fatal(e)
	}
	rows, e := s.KnowledgePages(ctx, a[1], imported.ID)
	if e != nil || len(rows) != 2 {
		t.Fatal(rows, e)
	}
	for _, x := range rows {
		if x.ID == p.ID || x.Parent == p.ID {
			t.Fatal("old IDs", x)
		}
	}
	b.Pages[0].Parent = "missing"
	before, _ := s.KnowledgeCollections(ctx, a[1])
	if _, e = s.ImportKnowledge(ctx, a[1], "", b); !errors.Is(e, ErrKnowledgeInvalid) {
		t.Fatal(e)
	}
	after, _ := s.KnowledgeCollections(ctx, a[1])
	if len(before) != len(after) {
		t.Fatal("partial import")
	}
	p.Archived = true
	p, e = s.SaveKnowledgePage(ctx, a[0], p, 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.PurgeKnowledgePage(ctx, a[0], c.ID, p.ID, p.Revision); !errors.Is(e, ErrKnowledgeInvalid) {
		t.Fatal("parent purged", e)
	}
	if e = s.UpdateKnowledgeCollection(ctx, a[0], c.ID, c.Title, "", true, 1); e != nil {
		t.Fatal(e)
	}
	if e = s.PurgeKnowledgeCollection(ctx, a[0], c.ID, 2); e != nil {
		t.Fatal(e)
	}
	if _, e = s.KnowledgePage(ctx, a[0], c.ID, p.ID, 1); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
}

func TestKnowledgeBackupAndBounds(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	team, e := s.CreateTeam(ctx, a[0], "Team", "")
	if e != nil {
		t.Fatal(e)
	}
	addTeamMember(t, s, a[0], team.ID, a[1], "member")
	c, e := s.CreateKnowledgeCollection(ctx, a[0], team.ID, "Shared", "")
	if e != nil {
		t.Fatal(e)
	}
	p := testPage(c, "Bounded", strings.Repeat("x", KnowledgeMaxBody+1))
	if _, e = s.SaveKnowledgePage(ctx, a[0], p, 0); !errors.Is(e, ErrKnowledgeInvalid) {
		t.Fatal(e)
	}
	p.Body = "Preserved text"
	p, e = s.SaveKnowledgePage(ctx, a[0], p, 0)
	if e != nil {
		t.Fatal(e)
	}
	tr, _, _, _ := s.Team(ctx, a[0], team.ID)
	if e = s.UpdateTeam(ctx, a[0], team.ID, "Team", "", true, tr.Revision); e != nil {
		t.Fatal(e)
	}
	if e = s.PurgeTeam(ctx, a[0], team.ID, tr.Revision+1); !errors.Is(e, ErrKnowledgeInvalid) {
		t.Fatal("team content destroyed", e)
	}
	filename := filepath.Join(t.TempDir(), "restore.db")
	if _, e = s.db.Exec(`VACUUM INTO ?`, filename); e != nil {
		t.Fatal(e)
	}
	restored, e := Open(filename)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	got, e := restored.KnowledgePage(ctx, a[1], c.ID, p.ID, 1)
	if e != nil || got.Body != p.Body || got.Sources != p.Sources {
		t.Fatal(got, e)
	}
	if _, e = restored.KnowledgePage(ctx, a[2], c.ID, p.ID, 1); !errors.Is(e, ErrNotFound) {
		t.Fatal("backup lost ACL", e)
	}
	if e = s.UpdateKnowledgeCollection(ctx, a[0], c.ID, c.Title, "", true, 1); !errors.Is(e, ErrKnowledgeReadOnly) {
		t.Fatal("archived team edited", e)
	}
	if e = s.UpdateTeam(ctx, a[0], team.ID, "Team", "", false, tr.Revision+1); e != nil {
		t.Fatal(e)
	}
	if e = s.UpdateKnowledgeCollection(ctx, a[0], c.ID, c.Title, "", true, 1); e != nil {
		t.Fatal(e)
	}
	if e = s.PurgeKnowledgeCollection(ctx, a[0], c.ID, 2); e != nil {
		t.Fatal(e)
	}
	if e = s.UpdateTeam(ctx, a[0], team.ID, "Team", "", true, tr.Revision+2); e != nil {
		t.Fatal(e)
	}
	if e = s.PurgeTeam(ctx, a[0], team.ID, tr.Revision+3); e != nil {
		t.Fatal(e)
	}
	var n int
	if e = s.db.QueryRow(`SELECT COUNT(*) FROM team_audit WHERE team_id=? AND action='delete'`, team.ID).Scan(&n); e != nil || n != 1 {
		t.Fatal("deletion audit", n, e)
	}
}

func TestKnowledgeCrossCollectionIDInvalid(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	first, err := s.CreateKnowledgeCollection(ctx, a[0], "", "First", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateKnowledgeCollection(ctx, a[1], "", "Second", "")
	if err != nil {
		t.Fatal(err)
	}
	p := testPage(first, "Private", "Evidence")
	if _, err = s.SaveKnowledgePage(ctx, a[0], p, 0); err != nil {
		t.Fatal(err)
	}
	p.CollectionID = second.ID
	if _, err = s.SaveKnowledgePage(ctx, a[1], p, 0); !errors.Is(err, ErrKnowledgeInvalid) {
		t.Fatal(err)
	}
}

func TestKnowledgeImportChunksAndQuota(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	bundle := KnowledgeBundle{Version: 1, Title: "Bulk"}
	parent := ""
	for i := 0; i < KnowledgeMaxPages; i++ {
		p := testPage(KnowledgeCollection{}, fmt.Sprint(i), strings.Repeat("x", 8<<10))
		p.Parent, p.Revision = parent, 1
		bundle.Pages = append(bundle.Pages, p)
		parent = p.ID
	}
	done := make(chan error, 1)
	go func() { _, err := s.ImportKnowledge(ctx, a[0], "", bundle); done <- err }()
	intermediate := false
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if !intermediate {
				t.Fatal("import did not release the database between chunks")
			}
			goto complete
		default:
		}
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM knowledge_pages p JOIN knowledge_collections c ON c.id=p.collection_id WHERE c.archived=1 AND c.revision=0`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count > 0 && count < KnowledgeMaxPages {
			intermediate = true
			if count%16 != 0 {
				t.Fatalf("partial chunk visible: %d", count)
			}
		}
	}
complete:
	collections, err := s.KnowledgeCollections(ctx, a[0])
	if err != nil || len(collections) != 1 || !collections[0].Writable {
		t.Fatal(collections, err)
	}
	bundle.Pages = append(bundle.Pages, KnowledgePage{ID: uuid.NewString(), Revision: 1, Title: "Over quota"})
	if _, err = s.ImportKnowledge(ctx, a[0], "", bundle); !errors.Is(err, ErrKnowledgeInvalid) {
		t.Fatal(err)
	}
	// Aggregate size includes the imported revision reasons.
	bundle.Pages = bundle.Pages[:129]
	for i := range bundle.Pages {
		bundle.Pages[i].Body = strings.Repeat("x", KnowledgeMaxBody)
	}
	if _, err = s.ImportKnowledge(ctx, a[0], "", bundle); !errors.Is(err, ErrKnowledgeInvalid) {
		t.Fatal(err)
	}
}

func TestKnowledgeImportFailureCleansCommittedChunks(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_import BEFORE INSERT ON knowledge_pages WHEN (SELECT count(*) FROM knowledge_pages)>=16 BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	b := KnowledgeBundle{Version: 1, Title: "Fail"}
	for range 17 {
		p := testPage(KnowledgeCollection{}, "Page", "body")
		p.Revision = 1
		b.Pages = append(b.Pages, p)
	}
	if _, err := s.ImportKnowledge(ctx, a[0], "", b); err == nil {
		t.Fatal("injected failure ignored")
	}
	for _, table := range []string{"knowledge_collections", "knowledge_pages", "knowledge_revisions", "knowledge_search"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatal(table, n, err)
		}
	}
}

func TestKnowledgeImportStagingReadOnly(t *testing.T) {
	s, a := knowledgeSetup(t)
	ctx := context.Background()
	c, err := s.CreateKnowledgeCollection(ctx, a[0], "", "Partial import", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE knowledge_collections SET revision=0,archived=1 WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateKnowledgeCollection(ctx, a[0], c.ID, c.Title, "", false, 0); !errors.Is(err, ErrKnowledgeReadOnly) {
		t.Fatal(err)
	}
	if _, err = s.SaveKnowledgePage(ctx, a[0], testPage(c, "Concurrent write", "body"), 0); !errors.Is(err, ErrKnowledgeReadOnly) {
		t.Fatal(err)
	}
	if err = s.PurgeKnowledgeCollection(ctx, a[0], c.ID, 0); err != nil {
		t.Fatal("cannot purge interrupted import", err)
	}
}
