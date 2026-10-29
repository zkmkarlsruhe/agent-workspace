// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

// A container that reports another person's session id gets its own row;
// the victim's snapshot, evaluation and cost stay untouched.
func TestChatsTenantScoped(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	for i, name := range []string{"alice", "bob"} {
		if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(i + 1), Username: name}); err != nil {
			t.Fatal(err)
		}
		s.AcceptConsent(ctx, uid(i+1), "v1", now)
	}
	victim := Chat{SessionID: "ses_victim00", UID: uid(1), Title: "Echt", Created: now, Updated: now, CollectedAt: now, Transcript: "ECHT"}
	if err := s.PutChat(ctx, victim); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutEval(ctx, uid(1), "ses_victim00", "v1", ChatEval{ChatUpdated: now, EvaluatedAt: now, Source: "llm", Result: "{}"}); err != nil {
		t.Fatal(err)
	}
	eur := 0.5
	s.InsertUsage(ctx, UsageEvent{EventID: "e1", Start: now, End: now, UID: uid(2), RootSessionID: "ses_victim00", Capability: "llm", Status: 200, CostEUR: &eur})
	forged := victim
	forged.UID, forged.Title, forged.Transcript, forged.Updated = uid(2), "Falsch", "FALSCH", now.Add(time.Second)
	if err := s.PutChat(ctx, forged); err != nil {
		t.Fatal(err)
	}
	c, err := s.ChatByID(ctx, "ses_victim00")
	if err != nil || c.UID != uid(1) || c.Transcript != "ECHT" || c.Eval == nil || c.CostEUR != 0 {
		t.Fatalf("victim snapshot changed: %+v %v", c, err)
	}
	if n, _ := s.ChatUpdates(ctx, uid(1)); n["ses_victim00"] != now.UnixMilli() {
		t.Fatalf("victim updates %v", n)
	}
}

// An evaluation that finishes after the chat changed, its consent changed or
// its account was deleted stores nothing.
func TestPutEvalConditional(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "alice"})
	s.AcceptConsent(ctx, uid(1), "v1", now)
	s.PutChat(ctx, Chat{SessionID: "ses_00000001", UID: uid(1), Created: now, Updated: now, CollectedAt: now, Transcript: "x"})
	evals := func() (n int) {
		s.db.QueryRow(`SELECT COUNT(*) FROM chat_evals`).Scan(&n)
		return n
	}
	if _, err := s.PutEval(ctx, uid(1), "ses_00000001", "v1", ChatEval{ChatUpdated: now.Add(-time.Minute), EvaluatedAt: now, Source: "llm", Result: "{}"}); err != nil || evals() != 0 {
		t.Fatalf("stale evaluation stored: %v %d", err, evals())
	}
	if ok, err := s.PutEval(ctx, uid(1), "ses_00000001", "v2", ChatEval{ChatUpdated: now, EvaluatedAt: now, Source: "llm", Result: "{}"}); ok || err != nil || evals() != 0 {
		t.Fatalf("evaluation stored without current consent: %v %v %d", ok, err, evals())
	}
	if ok, err := s.PutEval(ctx, uid(1), "ses_00000001", "v1", ChatEval{ChatUpdated: now, EvaluatedAt: now, Source: "llm", Result: "{}"}); !ok || err != nil || evals() != 1 {
		t.Fatalf("current evaluation not stored: %v %v %d", ok, err, evals())
	}
	if _, err := s.DeleteUser(ctx, uid(1), "geloescht-00"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutEval(ctx, uid(1), "ses_00000001", "v1", ChatEval{ChatUpdated: now, EvaluatedAt: now, Source: "llm", Result: "{}"}); err != nil || evals() != 0 {
		t.Fatalf("evaluation stored after deletion: %v %d", err, evals())
	}
	if _, err := s.PutEval(ctx, "geloescht-00", "ses_00000001", "v1", ChatEval{ChatUpdated: now, EvaluatedAt: now, Source: "llm", Result: "{}"}); err != nil || evals() != 0 {
		t.Fatalf("evaluation stored on the stub: %v %d", err, evals())
	}
}

// Migration 12 keeps chats and evaluations of a version-11 database.
func TestMigrateChatsV12(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:11] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	db.Exec(`PRAGMA user_version = 11`)
	db.Exec(`INSERT INTO users (uid, username, slug, slot, created_at) VALUES (?, 'alice', 'alice', 1, 0)`, uid(1))
	db.Exec(`INSERT INTO chats (session_id, uid, created, updated, collected_at, transcript) VALUES ('ses_00000001', ?, 0, 5, 0, 'T')`, uid(1))
	db.Exec(`INSERT INTO chat_evals (session_id, chat_updated, evaluated_at, source, result) VALUES ('ses_00000001', 5, 6, 'llm', '{}')`)
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.ChatByID(context.Background(), "ses_00000001")
	if err != nil || c.Transcript != "T" || c.Eval == nil || c.Eval.EvaluatedAt.UnixMilli() != 6 {
		t.Fatalf("%+v %v", c, err)
	}
	var fk string
	s.db.QueryRow(`SELECT "table" FROM pragma_foreign_key_list('chat_evals')`).Scan(&fk)
	if fk != "chats" {
		t.Fatalf("chat_evals references %q", fk)
	}
}
