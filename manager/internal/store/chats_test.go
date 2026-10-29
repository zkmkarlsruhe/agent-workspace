// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

// Account deletion: the person's evaluations go, the snapshots keep only
// anonymous counts, other people's chats stay untouched.
func TestDeleteUserChats(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	for i, name := range []string{"alice", "bob"} {
		if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(i + 1), Username: name}); err != nil {
			t.Fatal(err)
		}
		s.AcceptConsent(ctx, uid(i+1), "v1", now)
		id := "ses_chat0000" + strconv.Itoa(i+1)
		if err := s.PutChat(ctx, Chat{SessionID: id, UID: uid(i + 1), Project: "p", Title: "T", Created: now, Updated: now,
			CollectedAt: now, ToolErrors: 2, Truncated: true, Transcript: "GEHEIM"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutEval(ctx, uid(i+1), id, "v1", ChatEval{ChatUpdated: now, EvaluatedAt: now, Source: "llm", Result: `{"thema":"x"}`}); err != nil {
			t.Fatal(err)
		}
	}
	if c, err := s.ChatByID(ctx, "ses_chat00002"); err != nil || !c.Truncated || c.Eval == nil || c.Username != "bob" {
		t.Fatalf("%+v %v", c, err)
	}
	before, _ := s.DayStat(ctx, now.Add(-time.Hour), now.Add(time.Hour))
	d, err := s.DeleteUser(ctx, uid(2), "geloescht-0123456789abcdef")
	if err != nil || d.Chats != 1 {
		t.Fatal(d, err)
	}
	var evals int
	var rest string
	s.db.QueryRow(`SELECT COUNT(*) FROM chat_evals WHERE session_id = 'ses_chat00002'`).Scan(&evals)
	s.db.QueryRow(`SELECT uid || title || project || transcript FROM chats WHERE session_id = 'ses_chat00002'`).Scan(&rest)
	if evals != 0 || rest != "geloescht-0123456789abcdef" {
		t.Fatalf("evals %d, stub %q", evals, rest)
	}
	if _, err := s.ChatByID(ctx, "ses_chat00002"); err != ErrNotFound {
		t.Fatal("stub visible:", err)
	}
	if c, err := s.ChatByID(ctx, "ses_chat00001"); err != nil || c.Transcript != "GEHEIM" || c.Eval == nil {
		t.Fatalf("other person touched: %+v %v", c, err)
	}
	if after, _ := s.DayStat(ctx, now.Add(-time.Hour), now.Add(time.Hour)); after != before || after.ToolErrors != 4 {
		t.Fatalf("%+v -> %+v", before, after)
	}
}

// A container may report another person's session id. Purging the
// attacker's trash must not touch the victim's evaluation, and the admin
// lookup by id must pick the first collected row, whatever "created" the
// container claims.
func TestForeignSessionID(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	const id = "ses_victim1234"
	for i, name := range []string{"victim", "attacker"} {
		if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(i + 1), Username: name}); err != nil {
			t.Fatal(err)
		}
		s.AcceptConsent(ctx, uid(i+1), "v1", now)
		created := now
		if name == "attacker" {
			created = time.UnixMilli(0)
		}
		at := now.Add(time.Duration(i) * time.Minute) // the forged copy can only be collected later
		if err := s.PutChat(ctx, Chat{SessionID: id, UID: uid(i + 1), Created: created, Updated: now, CollectedAt: at, Transcript: name}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutEval(ctx, uid(i+1), id, "v1", ChatEval{ChatUpdated: now, EvaluatedAt: now, Source: "llm", Result: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	// the victim's chat collected again later: still the first collected
	if err := s.PutChat(ctx, Chat{SessionID: id, UID: uid(1), Created: now, Updated: now, CollectedAt: now.Add(2 * time.Minute), Transcript: "victim"}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.ChatByID(ctx, id); err != nil || c.Username != "victim" {
		t.Fatalf("lookup picked %q %v", c.Username, err)
	}
	p := Project{ID: "pa", UID: uid(2), Folder: "x", CreatedAt: now}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveToTrash(ctx, p, "ta", now, now, []TrashSession{{id, true}}, false, false); err != nil {
		t.Fatal(err)
	}
	// the victim purging the id of the attacker's trash must not reach it either
	if err := s.DeleteTrash(ctx, uid(1), "ta"); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM chat_evals WHERE session_id = ?`, id).Scan(&n)
	if n != 2 {
		t.Fatalf("foreign purge removed evaluations: %d left", n)
	}
	if err := s.DeleteTrash(ctx, uid(2), "ta"); err != nil {
		t.Fatal(err)
	}
	var victim, attacker int
	s.db.QueryRow(`SELECT COUNT(*) FROM chat_evals WHERE session_id = ? AND uid = ?`, id, uid(1)).Scan(&victim)
	s.db.QueryRow(`SELECT COUNT(*) FROM chat_evals WHERE session_id = ? AND uid = ?`, id, uid(2)).Scan(&attacker)
	if victim != 1 || attacker != 0 {
		t.Fatalf("victim evals %d, attacker evals %d", victim, attacker)
	}
}

func TestChatQuota(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	for i, name := range []string{"anna", "bert"} {
		if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(i + 1), Username: name}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range ChatQuota + 1 {
		if err := s.PutChat(ctx, Chat{UID: uid(1), SessionID: fmt.Sprintf("ses_%08d", i), Project: fmt.Sprint(i % 2), Title: "first"}); err != nil {
			t.Fatal(err)
		}
	}
	known, err := s.ChatUpdates(ctx, uid(1))
	if err != nil || len(known) != ChatQuota {
		t.Fatalf("count=%d err=%v", len(known), err)
	}
	if _, ok := known[fmt.Sprintf("ses_%08d", ChatQuota)]; ok {
		t.Fatal("overflow chat stored")
	}
	for _, u := range []string{uid(1), uid(2)} {
		if err := s.PutChat(ctx, Chat{UID: u, SessionID: "ses_00000000", Title: strings.Repeat("界\n", 300), Transcript: "updated"}); err != nil {
			t.Fatal(err)
		}
		var title, transcript string
		if err := s.db.QueryRow(`SELECT title, transcript FROM chats WHERE uid = ? AND session_id = ?`, u, "ses_00000000").Scan(&title, &transcript); err != nil {
			t.Fatal(err)
		}
		if len(title) > 200 || !utf8.ValidString(title) || strings.Contains(title, "\n") || transcript != "updated" {
			t.Fatalf("title=%q transcript=%q", title, transcript)
		}
	}
}
