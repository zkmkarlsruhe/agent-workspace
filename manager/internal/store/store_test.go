// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func uid(i int) string { return fmt.Sprintf("%064x", i) }

// Ping takes the write lock and must leave no transaction open behind it.
func TestPingWritable(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for range 2 {
		if err := s.Ping(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "alice"}); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureUserSlugAndSlot(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	want := []struct {
		slug string
		slot int
	}{{"alice", 1}, {"alice-2", 2}, {"alice-3", 3}}
	for i, w := range want {
		u, err := s.EnsureUser(ctx, identity.Identity{UID: uid(i), Username: []string{"Alice", "ALICE", "alice"}[i]})
		if err != nil {
			t.Fatal(err)
		}
		if u.Slug != w.slug || u.Slot != w.slot {
			t.Fatalf("user %d: got %s/%d want %s/%d", i, u.Slug, u.Slot, w.slug, w.slot)
		}
	}
	// A rename is a new account; the original slug and data stay with the old name.
	u, _ := s.EnsureUser(ctx, identity.Identity{UID: uid(9), Username: "admin"})
	if u.Slug != "admin-2" || u.Slot != 4 {
		t.Fatalf("got %+v", u)
	}
	u, _ = s.EnsureUser(ctx, identity.Identity{UID: uid(9), Username: "renamed", Email: "x@example.org"})
	if u.Slug != "renamed" || u.UID == uid(9) || u.Email != "x@example.org" {
		t.Fatalf("rename must create a separate account: %+v", u)
	}
	// freed slot is reused lowest-first
	if _, err := s.db.Exec(`DELETE FROM users WHERE uid = ?`, uid(1)); err != nil {
		t.Fatal(err)
	}
	u, _ = s.EnsureUser(ctx, identity.Identity{UID: uid(10), Username: "neu"})
	if u.Slot != 2 {
		t.Fatalf("want slot 2, got %d", u.Slot)
	}
	if got, _ := s.UserBySlot(ctx, 2); got.UID != uid(10) {
		t.Fatal("UserBySlot")
	}
	if _, err := s.UserByUID(ctx, "nope"); err != ErrNotFound {
		t.Fatal(err)
	}
}

func TestGenerationProjectsUsage(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "a1"})
	for want := int64(1); want <= 2; want++ {
		if g, err := s.NextGeneration(ctx, uid(1)); err != nil || g != want {
			t.Fatalf("gen %d %v", g, err)
		}
	}
	p := Project{ID: "p1", UID: uid(1), Folder: "bericht", CreatedAt: time.Now()}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.ID = "p2"
	if err := s.CreateProject(ctx, p); err != ErrExists {
		t.Fatalf("want ErrExists, got %v", err)
	}
	if ps, _ := s.Projects(ctx, uid(1)); len(ps) != 1 || ps[0].Folder != "bericht" {
		t.Fatalf("%+v", ps)
	}
	n := int64(12)
	err := s.InsertUsage(ctx, UsageEvent{EventID: "e1", Start: time.Now(), End: time.Now(), UID: uid(1),
		Capability: "llm", Status: 200, InTokens: &n})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUsageQueries(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	f := func(v float64) *float64 { return &v }
	for i, e := range []UsageEvent{
		{ProjectID: "p1", SessionID: "ses_a", RootSessionID: "ses_a", CostEUR: f(0.5), CO2LoG: f(1), CO2HiG: f(4)},
		{ProjectID: "p1", SessionID: "ses_sub", RootSessionID: "ses_a", CostEUR: f(0.25), CO2LoG: f(1), CO2HiG: f(2)},
		{ProjectID: "p1", SessionID: "ses_b", CostEUR: f(1)}, // no root header, unknown alias: CO2 NULL
		{ProjectID: "p2", SessionID: "ses_c", RootSessionID: "ses_c", CostEUR: f(2), CO2LoG: f(3), CO2HiG: f(9)},
		{ProjectID: "p1", SessionID: "ses_old", CostEUR: f(100), Start: now.Add(-60 * 24 * time.Hour)},
		{ProjectID: "p1", SessionID: "ses_x", CostEUR: f(100), UID: uid(2)},
	} {
		e.EventID, e.Capability, e.Status = fmt.Sprint(i), "llm", 200
		if e.Start.IsZero() {
			e.Start = now
		}
		if e.UID == "" {
			e.UID = uid(1)
		}
		e.End = e.Start
		if err := s.InsertUsage(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	since := now.Add(-30 * 24 * time.Hour)
	rows, err := s.Usage(ctx, uid(1), since)
	if err != nil || len(rows) != 2 {
		t.Fatalf("%+v %v", rows, err)
	}
	want := map[string]UsageRow{"p1": {ProjectID: "p1", CostEUR: 1.75, CO2LoG: 2, CO2HiG: 6, Sessions: 2},
		"p2": {ProjectID: "p2", CostEUR: 2, CO2LoG: 3, CO2HiG: 9, Sessions: 1}}
	for _, r := range rows {
		if r != want[r.ProjectID] {
			t.Errorf("project %+v, want %+v", r, want[r.ProjectID])
		}
	}
	ss, err := s.SessionUsage(ctx, uid(1), "p1", since)
	if err != nil || len(ss) != 2 {
		t.Fatalf("%+v %v", ss, err)
	}
	for _, r := range ss {
		if r.SessionID == "ses_a" && (r.CostEUR != 0.75 || r.CO2LoG != 2 || r.CO2HiG != 6) || r.SessionID == "ses_b" && r.CostEUR != 1 {
			t.Errorf("session %+v", r)
		}
	}
	if tot, err := s.UsageTotal(ctx, uid(1), since); err != nil || tot.CostEUR != 3.75 || tot.CO2HiG != 15 || tot.Sessions != 3 {
		t.Fatalf("%+v %v", tot, err)
	}
	// one chat with its subagent, of this person only
	if c, err := s.ChatUsage(ctx, uid(1), "ses_a", since); err != nil || c.CostEUR != 0.75 || c.CO2LoG != 2 || c.CO2HiG != 6 || c.Sessions != 1 {
		t.Fatalf("chat %+v %v", c, err)
	}
	if c, err := s.ChatUsage(ctx, uid(2), "ses_a", since); err != nil || c.Sessions != 0 {
		t.Fatalf("other person %+v %v", c, err)
	}
	if c, err := s.ChatUsage(ctx, uid(1), "ses_a", now.Add(time.Second)); err != nil || c.Sessions != 0 {
		t.Fatalf("since %+v %v", c, err)
	}
}

func TestTrashSessionsMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// roll back to schema 1 and reopen: migration 3 must run on an existing DB
	if _, err := s.db.Exec(`ALTER TABLE usage_events DROP COLUMN raw_cost; ALTER TABLE usage_events DROP COLUMN raw_cost_unit; ALTER TABLE usage_events DROP COLUMN raw_cost_source;
 DROP TRIGGER users_incarnation; ALTER TABLE users DROP COLUMN incarnation; DROP TABLE project_knowledge; DROP TABLE knowledge_revisions; DROP TABLE knowledge_pages; DROP TABLE knowledge_collections; DROP TABLE knowledge_search; DROP TABLE team_audit; DROP TABLE team_members; DROP TABLE teams; DROP TABLE assistant_config; DROP TABLE assistant_revisions; DROP TABLE assistant_gaps; DROP INDEX usage_events_ts; DROP TABLE opencode_profile_projects; DROP TABLE profile_sessions; DROP TABLE profile_projects; DROP INDEX users_username; DROP TABLE identity_releases;
		ALTER TABLE users DROP COLUMN identity_issuer; ALTER TABLE users DROP COLUMN identity_subject;
		ALTER TABLE users DROP COLUMN blocked_issuer; ALTER TABLE users DROP COLUMN blocked_subject;
		ALTER TABLE projects DROP COLUMN capabilities; ALTER TABLE trash DROP COLUMN capabilities;
 ALTER TABLE projects DROP COLUMN profile_id; ALTER TABLE projects DROP COLUMN profile_version;
        ALTER TABLE projects DROP COLUMN profile_hash; ALTER TABLE trash DROP COLUMN profile_id;
        ALTER TABLE trash DROP COLUMN profile_version; ALTER TABLE trash DROP COLUMN profile_hash;
        DROP TABLE trash_sessions; ALTER TABLE projects DROP COLUMN last_activity;
		ALTER TABLE trash DROP COLUMN reason; ALTER TABLE trash DROP COLUMN pending_move; ALTER TABLE users DROP COLUMN lang; ALTER TABLE nc_files DROP COLUMN own; PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if s.db.QueryRow(`PRAGMA user_version`).Scan(&v); v != len(migrations) {
		t.Fatalf("user_version %d", v)
	}
	s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "a1"})
	s.AcceptConsent(ctx, uid(1), "v1", time.Now())
	p := Project{ID: "p1", UID: uid(1), Folder: "bericht", CreatedAt: time.Now()}
	s.CreateProject(ctx, p)
	s.InsertUsage(ctx, UsageEvent{EventID: "e1", Start: time.Now(), End: time.Now(), UID: uid(1), ProjectID: "p1",
		SessionID: "ses_root0000", Capability: "llm", Status: 200})
	err = s.MoveToTrash(ctx, p, "t1", time.Now(), time.Now(), []TrashSession{{"ses_root0000", true}, {"ses_kid", false}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if tr, _ := s.TrashItem(ctx, uid(1), "t1"); tr.Chats != 1 {
		t.Fatalf("chats %d", tr.Chats)
	}
	if ss, _ := s.TrashSessions(ctx, "t1"); len(ss) != 2 || !ss[1].Root {
		t.Fatalf("%+v", ss)
	}
	now := time.Now()
	if err := s.PutChat(ctx, Chat{SessionID: "ses_root0000", UID: uid(1), Project: "p", Title: "Geheim", Created: now, Updated: now, CollectedAt: now, Transcript: "inhalt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutEval(ctx, uid(1), "ses_root0000", "v1", ChatEval{ChatUpdated: now, EvaluatedAt: now, Source: "llm", Result: "{}"}); err != nil {
		t.Fatal(err)
	}
	s.DeleteTrash(ctx, uid(1), "t1")
	if ss, _ := s.TrashSessions(ctx, "t1"); len(ss) != 0 {
		t.Fatal("cascade")
	}
	// purge anonymises the collected snapshot and drops its evaluation
	var title, tr string
	var evals int
	s.db.QueryRow(`SELECT title, transcript FROM chats WHERE session_id = 'ses_root0000'`).Scan(&title, &tr)
	s.db.QueryRow(`SELECT count(*) FROM chat_evals`).Scan(&evals)
	if title != "" || tr != "" || evals != 0 {
		t.Fatalf("snapshot after purge: %q %q evals %d", title, tr, evals)
	}
}

func TestRenameProject(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "a1"})
	for _, p := range []Project{{ID: "p1", UID: uid(1), Folder: "alt"}, {ID: "p2", UID: uid(1), Folder: "belegt"}} {
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RenameProject(ctx, "p1", "belegt"); err != ErrExists {
		t.Fatalf("want ErrExists, got %v", err)
	}
	if err := s.RenameProject(ctx, "p1", "neu"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.ProjectByFolder(ctx, uid(1), "neu"); err != nil || p.ID != "p1" {
		t.Fatalf("%+v %v", p, err)
	}
	if err := s.RenameProject(ctx, "fehlt", "x"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
