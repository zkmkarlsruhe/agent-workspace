// SPDX-License-Identifier: MIT

package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Migration 20 rewrites the German ids and JSON keys stored before the
// English rename (docs/RENAME-ENGLISH.md 2e, 2f). Every step only touches old
// values, so running it on rewritten rows changes nothing. Uids of deleted
// accounts ("geloescht-<hex>") stay as they are.

// catalogIDs are the catalog item ids of catalog_optins.item.
var catalogIDs = map[string]string{
	"web-recherche": "web-research", "als-pdf": "md-to-pdf", "raeume": "rooms",
	"workspace-wissen": "workspace-knowledge", "bibliothek": "library", "medien": "media",
	"bildbeschreibung": "alt-text", "brief-mail": "letter-email", "einfache-sprache": "plain-language",
	"praesentation": "presentation", "pressetext": "press-release", "protokoll": "minutes",
	"reisekosten": "travel-expenses", "uebersetzen": "translate", "veranstaltung": "event-planning",
}

// valueRenames are plain column value rewrites: table, column, extra
// condition, old -> new.
var valueRenames = []struct {
	table, column, where string
	values               map[string]string
}{
	{"feedback", "kind", "", map[string]string{"idee": "idea", "lob": "praise", "frage": "question"}},
	// toolbox target is the catalog id; before the capability itself is renamed
	{"usage_events", "target", "capability = 'werkzeug'", map[string]string{"medien": "media"}},
	{"usage_events", "target", "capability = 'fetch'", map[string]string{"direkt": "direct"}},
	{"usage_events", "target", "capability IN ('bild', 'nextcloud')", map[string]string{
		"bild_ansehen": "image_view", "bild_generieren": "image_generate",
		"nextcloud.liste": "nextcloud.list", "nextcloud.holen": "nextcloud.fetch", "nextcloud.ablegen": "nextcloud.upload",
		"nextcloud.suchen": "nextcloud.search", "nextcloud.aktualisieren": "nextcloud.pull",
	}},
	{"usage_events", "capability", "", map[string]string{"raeume": "rooms", "bild": "image", "werkzeug": "toolbox", "mcp-zentral": "mcp-central"}},
	{"trash", "reason", "", map[string]string{"inaktiv": "idle"}},
	{"chat_evals", "source", "", map[string]string{"heuristik": "heuristic"}},
}

// jsonKey renames one JSON object key; values maps string values (also inside
// arrays), nested renames the keys of objects inside an array value.
type jsonKey struct {
	old, new string
	values   map[string]string
	nested   []jsonKey
}

var answerKeys = []jsonKey{
	{old: "anrede", new: "address_form", values: map[string]string{"du": "informal", "sie": "formal"}},
	{old: "sprache", new: "language", values: map[string]string{"deutsch": "de", "englisch": "en"}},
	{old: "tonfall", new: "tone", values: map[string]string{"sachlich": "factual", "freundlich": "friendly", "locker": "casual"}},
	{old: "laenge", new: "length", values: map[string]string{"kurz": "short", "mittel": "medium", "ausfuehrlich": "detailed"}},
	{old: "formate", new: "formats"},
	{old: "fachgebiet", new: "field"},
	{old: "aufgaben", new: "tasks", values: map[string]string{"recherche": "research", "tabellen": "spreadsheets", "texte": "writing",
		"praesentationen": "presentations", "korrektur": "proofreading", "uebersetzen": "translation"}},
	{old: "immer", new: "always"},
	{old: "nie", new: "never"},
	{old: "notiz", new: "note"},
}

var factKeys = []jsonKey{
	{old: "abteilung", new: "department"}, {old: "funktion", new: "role"}, {old: "telefon", new: "phone"},
	{old: "raum", new: "room"}, {old: "gruppen", new: "groups"}, {old: "stand", new: "as_of"},
}

var evalKeys = []jsonKey{
	{old: "thema", new: "topic"},
	{old: "results", new: "outcome"},
	{old: "erfolgreich", new: "success", values: map[string]string{"ja": "yes", "nein": "no", "teilweise": "partly"}},
	{old: "probleme", new: "problems", values: map[string]string{"werkzeugfehler": "tool-error", "missverstaendnis": "misunderstanding",
		"abbruch": "aborted", "modell-fehler": "model-error"}},
	{old: "hinweise_private_daten", new: "private_data"},
	{old: "begruendung_private_daten", new: "private_data_reason"},
	{old: "hinweise_missbrauch", new: "misuse"},
	{old: "begruendung_missbrauch", new: "misuse_reason"},
	{old: "verbesserungsideen", new: "improvements", nested: []jsonKey{{old: "art", new: "kind", values: map[string]string{
		"skill-fehlt": "missing-skill", "werkzeug-fehlt": "missing-tool", "prompt-unklar": "unclear-prompt",
		"anleitung-fehlt": "missing-guide", "sonstiges": "other"}}}},
}

func migrateEnglish(tx *sql.Tx) error {
	for _, c := range []struct{ table, old, new string }{{"nc_links", "alles", "sync_all"}, {"feedback", "art", "kind"}} {
		has, err := hasColumn(tx, c.table, c.old)
		if err != nil {
			return err
		}
		if has {
			if _, err := tx.Exec(`ALTER TABLE ` + c.table + ` RENAME COLUMN ` + c.old + ` TO ` + c.new); err != nil {
				return err
			}
		}
	}
	oldT, err := hasTable(tx, "nc_ablagen")
	if err != nil {
		return err
	}
	newT, err := hasTable(tx, "nc_uploads")
	if err != nil {
		return err
	}
	switch {
	case oldT && newT:
		if _, err := tx.Exec(`INSERT OR IGNORE INTO nc_uploads (uid, path, etag) SELECT uid, path, etag FROM nc_ablagen; DROP TABLE nc_ablagen`); err != nil {
			return err
		}
	case oldT:
		if _, err := tx.Exec(`ALTER TABLE nc_ablagen RENAME TO nc_uploads`); err != nil {
			return err
		}
	}
	// an old and a new opt-in of the same person collapse into the new one
	for old, nu := range catalogIDs {
		if _, err := tx.Exec(`UPDATE OR IGNORE catalog_optins SET item = ? WHERE item = ?`, nu, old); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM catalog_optins WHERE item = ?`, old); err != nil {
			return err
		}
	}
	for _, r := range valueRenames {
		where := ""
		if r.where != "" {
			where = " AND " + r.where
		}
		for old, nu := range r.values {
			if _, err := tx.Exec(`UPDATE `+r.table+` SET `+r.column+` = ? WHERE `+r.column+` = ?`+where, nu, old); err != nil {
				return err
			}
		}
	}
	// mcp-central targets are "<catalog id>/<tool>"
	for old, nu := range catalogIDs {
		if _, err := tx.Exec(`UPDATE usage_events SET target = ? || substr(target, ?) WHERE capability = 'mcp-central' AND target LIKE ? || '/%'`,
			nu, len(old)+1, old); err != nil {
			return err
		}
	}
	for _, c := range []struct {
		table, key, column string
		keys               []jsonKey
	}{
		{"profiles", "uid", "answers", answerKeys},
		{"profiles", "uid", "facts", factKeys},
		{"chat_evals", "uid || ' ' || session_id", "result", evalKeys},
	} {
		if err := rewriteJSONColumn(tx, c.table, c.key, c.column, c.keys); err != nil {
			return fmt.Errorf("%s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

func hasColumn(tx *sql.Tx, table, column string) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	return n > 0, err
}

func hasTable(tx *sql.Tx, table string) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
	return n > 0, err
}

// rewriteJSONColumn renames the keys of every JSON object in table.column;
// key is an SQL expression that identifies the row. NULL and rows that do
// not change stay untouched.
func rewriteJSONColumn(tx *sql.Tx, table, key, column string, keys []jsonKey) error {
	rows, err := tx.Query(`SELECT ` + key + `, ` + column + ` FROM ` + table + ` WHERE ` + column + ` IS NOT NULL`)
	if err != nil {
		return err
	}
	changed := map[string]string{}
	for rows.Next() {
		var k, raw string
		if err := rows.Scan(&k, &raw); err != nil {
			rows.Close()
			return err
		}
		d := json.NewDecoder(bytes.NewReader([]byte(raw)))
		d.UseNumber()
		var m map[string]any
		if d.Decode(&m) != nil || m == nil {
			continue // not an object: nothing the code reads, leave it
		}
		if renameKeys(m, keys) {
			b, err := json.Marshal(m)
			if err != nil {
				rows.Close()
				return err
			}
			changed[k] = string(b)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for k, v := range changed {
		if _, err := tx.Exec(`UPDATE `+table+` SET `+column+` = ? WHERE `+key+` = ?`, v, k); err != nil {
			return err
		}
	}
	return nil
}

// renameKeys applies keys to m and reports whether anything changed.
func renameKeys(m map[string]any, keys []jsonKey) bool {
	changed := false
	for _, k := range keys {
		v, ok := m[k.old]
		if !ok {
			continue
		}
		delete(m, k.old)
		if _, exists := m[k.new]; !exists {
			m[k.new] = mapValue(v, k)
		}
		changed = true
	}
	return changed
}

func mapValue(v any, k jsonKey) any {
	switch x := v.(type) {
	case string:
		if n, ok := k.values[x]; ok {
			return n
		}
	case []any:
		for i, e := range x {
			if o, ok := e.(map[string]any); ok {
				renameKeys(o, k.nested)
			} else {
				x[i] = mapValue(e, k)
			}
		}
	}
	return v
}
