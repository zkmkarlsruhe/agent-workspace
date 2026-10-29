// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/settings"
)

// Migration 20 on a version-19 database with the German names and values:
// columns and table renamed, ids, kinds and JSON keys rewritten, and the
// typed readers see the English values.
func TestMigrateEnglishV20(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:19] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`PRAGMA user_version = 19`)
	exec(`INSERT INTO users (uid, username, slug, slot, created_at) VALUES (?, 'alice', 'alice', 1, 0)`, uid(1))
	exec(`INSERT INTO projects (id, uid, folder, created_at) VALUES ('p1', ?, 'bericht', 0)`, uid(1))
	exec(`INSERT INTO nc_links (project_id, uid, nc_folder, alles, linked_at) VALUES ('p1', ?, 'bericht', 1, 0)`, uid(1))
	exec(`INSERT INTO nc_ablagen (uid, path, etag) VALUES (?, 'Agent Workspace/a.pdf', 'e1')`, uid(1))
	for _, it := range []string{"raeume", "medien", "protokoll", "minutes", "eigenes-ding"} {
		exec(`INSERT INTO catalog_optins (uid, item) VALUES (?, ?)`, uid(1), it)
	}
	for i, k := range []string{"problem", "idee", "lob", "frage"} {
		exec(`INSERT INTO feedback (ts, uid, source, art, text) VALUES (?, ?, 'chat', ?, 'x')`, i, uid(1), k)
	}
	for i, ct := range [][2]string{{"raeume", "mrbs"}, {"bild", "bild_ansehen"}, {"bild", "bild_generieren"}, {"werkzeug", "medien"},
		{"nextcloud", "nextcloud.aktualisieren"}, {"fetch", "direkt"}, {"mcp-zentral", "bild_ansehen"}, {"llm", "workspace/standard"}, {"mcp-zentral", "bibliothek/catalog_search"}} {
		exec(`INSERT INTO usage_events (event_id, ts_start, ts_end, uid, capability, target, status) VALUES (?, 0, 0, ?, ?, ?, 200)`,
			string(rune('a'+i)), uid(1), ct[0], ct[1])
	}
	exec(`INSERT INTO trash (id, uid, project_id, folder, created_at, deleted_at, purge_after, reason) VALUES ('t1', ?, 'p0', 'alt', 0, 0, 0, 'inaktiv')`, uid(1))
	exec(`INSERT INTO profiles (uid, facts, answers, text) VALUES (?, ?, ?, 'T')`, uid(1),
		`{"name":"Alice","email":"m@example.org","abteilung":"Operations","funktion":"Leitung","telefon":"1","raum":"B 2","gruppen":["a"],"stand":"2026-10-17T00:00:00Z"}`,
		`{"anrede":"sie","sprache":"englisch","tonfall":"locker","laenge":"ausfuehrlich","formate":["pdf","word"],"fachgebiet":"Technik","aufgaben":["recherche","korrektur"],"immer":"a","nie":"b","notiz":"c"}`)
	exec(`INSERT INTO chats (session_id, uid, created, updated, collected_at) VALUES ('ses_1', ?, 0, 5, 0)`, uid(1))
	exec(`INSERT INTO chats (session_id, uid, created, updated, collected_at) VALUES ('ses_2', ?, 0, 5, 0)`, uid(1))
	exec(`INSERT INTO chat_evals (uid, session_id, chat_updated, evaluated_at, source, result) VALUES (?, 'ses_1', 5, 6, 'llm', ?)`, uid(1),
		`{"thema":"Pressetext","results":"fertig","erfolgreich":"teilweise","probleme":["werkzeugfehler","abbruch"],"hinweise_private_daten":true,
		"begruendung_private_daten":"Gehalt","hinweise_missbrauch":false,"begruendung_missbrauch":"","verbesserungsideen":[{"art":"skill-fehlt","text":"Vorlage"}]}`)
	exec(`INSERT INTO chat_evals (uid, session_id, chat_updated, evaluated_at, source, result) VALUES (?, 'ses_2', 5, 6, 'heuristik', '{"thema":"x","results":"","erfolgreich":"","probleme":null}')`, uid(1))
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if s.db.QueryRow(`PRAGMA user_version`).Scan(&v); v != len(migrations) {
		t.Fatalf("user_version %d of %d", v, len(migrations))
	}
	strs := func(q string, args ...any) []string {
		t.Helper()
		rows, err := s.db.Query(q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var x string
			rows.Scan(&x)
			out = append(out, x)
		}
		return out
	}
	want := func(name string, got, w []string) {
		t.Helper()
		if !slices.Equal(got, w) {
			t.Errorf("%s: %q, want %q", name, got, w)
		}
	}
	want("optins", strs(`SELECT item FROM catalog_optins ORDER BY item`), []string{"eigenes-ding", "media", "minutes", "rooms"})
	want("feedback", strs(`SELECT kind FROM feedback ORDER BY ts`), []string{"problem", "idea", "praise", "question"})
	want("usage", strs(`SELECT capability || ' ' || target FROM usage_events ORDER BY event_id`), []string{"rooms mrbs", "image image_view",
		"image image_generate", "toolbox media", "nextcloud nextcloud.pull", "fetch direct", "mcp-central bild_ansehen", "llm workspace/standard", "mcp-central library/catalog_search"})
	want("trash", strs(`SELECT reason FROM trash`), []string{"idle"})
	want("sources", strs(`SELECT source FROM chat_evals ORDER BY session_id`), []string{"llm", "heuristic"})

	if l, err := s.NCLink(ctx, "p1"); err != nil || !l.All {
		t.Errorf("nc link: %+v %v", l, err)
	}
	if e, err := s.SyncETag(ctx, uid(1), "Agent Workspace/a.pdf"); err != nil || e != "e1" {
		t.Errorf("nc_uploads: %q %v", e, err)
	}
	if fs, err := s.FeedbackList(ctx, 10); err != nil || len(fs) != 4 || fs[0].Kind != "question" {
		t.Errorf("feedback rows: %+v %v", fs, err)
	}
	d, err := s.LoadSettings(ctx, uid(1))
	if err != nil || d.Answers == nil {
		t.Fatalf("profile: %+v %v", d, err)
	}
	a := d.Answers
	if a.AddressForm != "formal" || a.Language != "en" || a.Tone != "casual" || a.Length != "detailed" || a.Field != "Technik" ||
		!slices.Equal(a.Tasks, []settings.Task{"research", "proofreading"}) || len(a.Formats) != 2 || a.Always != "a" || a.Never != "b" || a.Note != "c" {
		t.Errorf("answers: %+v", a)
	}
	if f := d.Facts; f.Department != "Operations" || f.Role != "Leitung" || f.Phone != "1" || f.Room != "B 2" ||
		!slices.Equal(f.Groups, []string{"a"}) || f.AsOf.IsZero() || f.Name != "Alice" {
		t.Errorf("facts: %+v", f)
	}
	var res map[string]any
	json.Unmarshal([]byte(strs(`SELECT result FROM chat_evals WHERE session_id = 'ses_1'`)[0]), &res)
	wantEval := map[string]any{"topic": "Pressetext", "outcome": "fertig", "success": "partly", "problems": []any{"tool-error", "aborted"},
		"private_data": true, "private_data_reason": "Gehalt", "misuse": false, "misuse_reason": "",
		"improvements": []any{map[string]any{"kind": "missing-skill", "text": "Vorlage"}}}
	if !reflect.DeepEqual(res, wantEval) {
		t.Errorf("eval: %v", res)
	}

	// the step is safe to repeat on rewritten data and the new schema
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`CREATE TABLE nc_ablagen (uid TEXT NOT NULL, path TEXT NOT NULL, etag TEXT NOT NULL, PRIMARY KEY (uid, path));
		INSERT INTO nc_ablagen VALUES ('u', 'p', 'e2')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateEnglish(tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	want("optins again", strs(`SELECT item FROM catalog_optins ORDER BY item`), []string{"eigenes-ding", "media", "minutes", "rooms"})
	want("uploads merged", strs(`SELECT etag FROM nc_uploads ORDER BY etag`), []string{"e1", "e2"})
	var res2 map[string]any
	json.Unmarshal([]byte(strs(`SELECT result FROM chat_evals WHERE session_id = 'ses_1'`)[0]), &res2)
	if !reflect.DeepEqual(res2, wantEval) {
		t.Errorf("eval after repeat: %v", res2)
	}
}
