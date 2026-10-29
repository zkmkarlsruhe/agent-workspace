// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageRawBilling(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "accounting.db")
	s, err := Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	old := UsageEvent{EventID: "old", UID: "billing-test", Capability: "llm", Start: time.Now(), End: time.Now(), Status: 200, CostEUR: new(.1), CostSource: "estimate"}
	if err := s.InsertUsage(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-migration schema with an existing ledger row.
	if _, err := s.db.Exec(`DROP TRIGGER users_incarnation; ALTER TABLE users DROP COLUMN incarnation; DROP TABLE project_knowledge; DROP TABLE knowledge_revisions; DROP TABLE knowledge_pages; DROP TABLE knowledge_collections; DROP TABLE knowledge_search; DROP TABLE team_audit; DROP TABLE team_members; DROP TABLE teams; DROP TABLE assistant_config; DROP TABLE assistant_revisions; DROP TABLE assistant_gaps; ALTER TABLE usage_events DROP COLUMN raw_cost;
 ALTER TABLE usage_events DROP COLUMN raw_cost_unit;
 ALTER TABLE usage_events DROP COLUMN raw_cost_source;
 PRAGMA user_version = 26;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var preserved bool
	if err := s.db.QueryRow(`SELECT cost_eur = 0.1 AND raw_cost IS NULL AND raw_cost_unit IS NULL FROM usage_events WHERE event_id='old'`).Scan(&preserved); err != nil || !preserved {
		t.Fatalf("old row changed: %v", err)
	}
	e := UsageEvent{EventID: "raw", UID: "billing-test", Capability: "embedding", Start: time.Now(), End: time.Now(), Status: 200,
		CostEUR: new(.2), CostSource: "usage-usd", RawCost: new(.25), RawCostUnit: "USD", RawCostSource: "usage"}
	if err := s.InsertUsage(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	var raw, eur float64
	var unit, source, rawSource string
	if err := s.db.QueryRow(`SELECT raw_cost, raw_cost_unit, raw_cost_source, cost_eur, cost_source FROM usage_events WHERE event_id='raw'`).Scan(&raw, &unit, &rawSource, &eur, &source); err != nil {
		t.Fatal(err)
	}
	if raw != .25 || eur != .2 || unit != "USD" || source != "usage-usd" || rawSource != "usage" {
		t.Fatalf("%g %g %s %s %s", raw, eur, unit, source, rawSource)
	}
}
