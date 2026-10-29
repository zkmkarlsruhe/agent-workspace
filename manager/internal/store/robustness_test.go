// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func TestUsageTimeIndexMigration(t *testing.T) {
	s := open(t)
	if _, err := s.db.Exec(`ALTER TABLE usage_events DROP COLUMN raw_cost; ALTER TABLE usage_events DROP COLUMN raw_cost_unit; ALTER TABLE usage_events DROP COLUMN raw_cost_source; DROP INDEX usage_events_ts; PRAGMA user_version = 25`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != len(migrations) {
		t.Fatal("migration version", version, err)
	}
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT COUNT(DISTINCT uid), COUNT(DISTINCT root_session_id), SUM(cost_eur)
		FROM usage_events WHERE ts_start >= ? AND ts_start < ? AND uid != ?`, 0, 1, ManagerUID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	used := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		used = used || strings.Contains(detail, "USING INDEX usage_events_ts")
	}
	if err := rows.Err(); err != nil || !used {
		t.Fatal("daily query did not use time index", err)
	}
}

func TestDeleteTrashRemovesNextcloudState(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	p := Project{ID: "linked", UID: uid(1), Folder: "linked", CreatedAt: now}
	for _, err := range []error{
		s.CreateProject(ctx, p),
		s.PutNCLink(ctx, p.ID, p.UID, p.Folder, true, now),
		s.PutNCFile(ctx, p.ID, NCFile{Rel: "file.txt", ETag: "tag"}),
		s.AddNCCollection(ctx, p.ID, p.UID, "documents", now),
		s.MoveToTrash(ctx, p, "trash", now, now, nil, false, false),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, owner := range []string{uid(2), p.UID} {
		if err := s.DeleteTrash(ctx, owner, "trash"); err != nil {
			t.Fatal(err)
		}
		want := 0
		if owner != p.UID {
			want = 1
		}
		for _, table := range []string{"nc_links", "nc_files", "nc_collections"} {
			var n int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE project_id = ?`, p.ID).Scan(&n); err != nil || n != want {
				t.Fatalf("%s rows after %s purge: %d, %v", table, owner, n, err)
			}
		}
	}
}

func TestPutChatValidatesSessionID(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id    string
		valid bool
	}{
		{"ses_1234567", false},
		{"ses_12345678", true},
		{"ses_" + strings.Repeat("A", 80), true},
		{"ses_" + strings.Repeat("A", 81), false},
		{"ses_../../victim", false},
		{"ses_12345678\n", false},
		{"ses_profile_forged", false},
	} {
		if err := s.PutChat(ctx, Chat{UID: uid(1), SessionID: tc.id}); err != nil {
			t.Fatal(err)
		}
		var stored bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM chats WHERE session_id = ?)`, tc.id).Scan(&stored); err != nil || stored != tc.valid {
			t.Fatalf("%q stored=%v, want %v: %v", tc.id, stored, tc.valid, err)
		}
	}
}

func TestPutAssistantChatRejectsUnknownSession(t *testing.T) {
	s := open(t)
	if err := s.PutAssistantChat(context.Background(), Chat{UID: uid(1), SessionID: "ses_profile_forged"}); err != ErrNotFound {
		t.Fatal("unknown assistant session accepted", err)
	}
}
