// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

// Feedback: rate limit per person, list newest first without the excerpt,
// done mark, excerpt cleared with its trashed chat, rows gone with the account.
func TestFeedback(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	for i, name := range []string{"alice", "bob"} {
		if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(i + 1), Username: name}); err != nil {
			t.Fatal(err)
		}
	}
	add := func(u, session, text string, at time.Time) (int64, error) {
		return s.AddFeedback(ctx, Feedback{At: at, UID: u, Source: "chat", Kind: "problem", Text: text, Project: "p",
			SessionID: session, Context: "AUSZUG " + text}, now.Add(-24*time.Hour), 2)
	}
	if _, err := add(uid(1), "ses_alt", "gestern", now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	a, err1 := add(uid(1), "ses_a", "eins", now.Add(-time.Minute))
	_, err2 := add(uid(1), "ses_b", "zwei", now)
	if _, err := add(uid(1), "ses_b", "drei", now); err1 != nil || err2 != nil || err != ErrLimit {
		t.Fatalf("limit: %v %v %v", err1, err2, err)
	}
	if _, err := add(uid(2), "ses_o", "bob", now); err != nil {
		t.Fatal("the limit is per person:", err)
	}
	l, err := s.FeedbackList(ctx, 10)
	if err != nil || len(l) != 4 || l[3].Text != "gestern" || l[0].Context != "" || l[3].Username != "alice" {
		t.Fatalf("%+v %v", l, err)
	}
	if err := s.SetFeedbackDone(ctx, a, true); err != nil {
		t.Fatal(err)
	}
	if f, err := s.FeedbackByID(ctx, a); err != nil || !f.Done || f.Context != "AUSZUG eins" {
		t.Fatalf("%+v %v", f, err)
	}
	if err := s.SetFeedbackDone(ctx, 999, true); err != ErrNotFound {
		t.Fatal(err)
	}

	s.CreateProject(ctx, Project{ID: "p1", UID: uid(1), Folder: "p"})
	s.MoveToTrash(ctx, Project{ID: "p1", UID: uid(1), Folder: "p"}, "t1", now, now, []TrashSession{{"ses_a", true}}, false, false)
	if err := s.DeleteTrash(ctx, uid(1), "t1"); err != nil {
		t.Fatal(err)
	}
	if f, _ := s.FeedbackByID(ctx, a); f.Context != "" || f.Text != "eins" {
		t.Fatalf("after DeleteTrash: %+v", f)
	}

	if _, err := s.DeleteUser(ctx, uid(1), "geloescht-x"); err != nil {
		t.Fatal(err)
	}
	if l, _ = s.FeedbackList(ctx, 10); len(l) != 1 || l[0].UID != uid(2) {
		t.Fatalf("after DeleteUser: %+v", l)
	}
	if err := s.AuditFeedbackRead(ctx, now, uid(2), "bob", l[0].ID); err != nil {
		t.Fatal(err)
	}
	if au, _ := s.Audit(ctx, 1); len(au) != 1 || au[0].Do != "feedback_read" {
		t.Fatalf("%+v", au)
	}
}

// A feedback captured before its chat was deleted for good must not bring the
// excerpt back when it is stored afterwards (feedback_send vs DeleteTrash).
func TestFeedbackAfterPermanentDeletion(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	s.CreateProject(ctx, Project{ID: "p1", UID: uid(1), Folder: "p"})
	s.MoveToTrash(ctx, Project{ID: "p1", UID: uid(1), Folder: "p"}, "t1", now, now, []TrashSession{{"ses_a", true}, {"ses_kind", false}}, false, false)
	if err := s.DeleteTrash(ctx, uid(1), "t1"); err != nil {
		t.Fatal(err)
	}
	for _, se := range []string{"ses_a", "ses_kind"} {
		id, err := s.AddFeedback(ctx, Feedback{At: now, UID: uid(1), Source: "chat", Kind: "problem", Text: "spaet", Project: "p",
			SessionID: se, Context: "AUSZUG"}, now.Add(-24*time.Hour), 10)
		if err != nil {
			t.Fatal(err)
		}
		if f, _ := s.FeedbackByID(ctx, id); f.Context != "" || f.Text != "spaet" {
			t.Fatalf("%s: excerpt of a deleted chat stored: %+v", se, f)
		}
	}
	// another person's chat with the same id is not affected
	s.EnsureUser(ctx, identity.Identity{UID: uid(2), Username: "bob"})
	id, _ := s.AddFeedback(ctx, Feedback{At: now, UID: uid(2), Source: "chat", Kind: "problem", Text: "x", SessionID: "ses_a", Context: "AUSZUG"},
		now.Add(-24*time.Hour), 10)
	if f, _ := s.FeedbackByID(ctx, id); f.Context != "AUSZUG" {
		t.Fatalf("other person: %+v", f)
	}
}
