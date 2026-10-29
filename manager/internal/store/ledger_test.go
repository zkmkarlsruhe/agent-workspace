// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

// Deleting and recreating the account does not reset the day's spend.
func TestSpentSurvivesDeletion(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "alice"})
	for i, eur := range []float64{3, 1.5} {
		s.InsertUsage(ctx, UsageEvent{EventID: string(rune('a' + i)), Start: now, End: now, UID: uid(1), Capability: "llm", Status: 200, CostEUR: &eur})
	}
	old := 9.0
	s.InsertUsage(ctx, UsageEvent{EventID: "old", Start: now.Add(-72 * time.Hour), End: now, UID: uid(1), Capability: "llm", Status: 200, CostEUR: &old})
	if _, err := s.DeleteUser(ctx, uid(1), "geloescht-1"); err != nil {
		t.Fatal(err)
	}
	s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "alice"})
	day := now.Add(-time.Hour)
	if eur, err := s.SpentSince(ctx, uid(1), day); err != nil || eur != 4.5 {
		t.Fatalf("spent after re-creation: %v %v", eur, err)
	}
	if tot, _ := s.UsageTotal(ctx, uid(1), time.Time{}); tot.CostEUR != 0 {
		t.Fatal("usage must stay anonymised")
	}
	if eur, _ := s.SpentSince(ctx, uid(2), day); eur != 0 {
		t.Fatal("other person charged")
	}
	var old2 int
	s.db.QueryRow(`SELECT COUNT(*) FROM spend_ledger WHERE ts < ?`, now.Add(-48*time.Hour).UnixMilli()).Scan(&old2)
	if old2 != 0 {
		t.Fatal("ledger keeps more than two days")
	}
}
