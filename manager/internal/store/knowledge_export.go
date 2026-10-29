// SPDX-License-Identifier: MIT

package store

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// KnowledgeBundle contains portable content, never memberships or account credentials.
// Import restores current pages with new IDs; revision history remains in the export.
type KnowledgeBundle struct {
	Version     int             `json:"version"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Pages       []KnowledgePage `json:"pages"`
}

func exportKnowledge(ctx context.Context, tx *sql.Tx, a KnowledgeActor, id string) (b KnowledgeBundle, err error) {
	c, e := collectionFor(ctx, tx, a, id)
	if e != nil {
		return b, e
	}
	if c.Role != "admin" {
		return b, ErrNotFound
	}
	b = KnowledgeBundle{Version: 1, Title: c.Title, Description: c.Description}
	rows, e := tx.QueryContext(ctx, `SELECT `+pageColumns+` FROM knowledge_pages p JOIN knowledge_revisions r ON r.page_id=p.id WHERE p.collection_id=? ORDER BY p.id,r.revision`, id)
	if e != nil {
		return b, e
	}
	defer rows.Close()
	for rows.Next() {
		p, e := scanKnowledgePage(rows)
		if e != nil {
			return b, e
		}
		p.Actor = ""
		p.CollectionID = ""
		b.Pages = append(b.Pages, p)
	}
	return b, rows.Err()
}
func (s *Store) ExportKnowledge(ctx context.Context, a KnowledgeActor, id string) (b KnowledgeBundle, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error { var e error; b, e = exportKnowledge(ctx, tx, a, id); return e })
	return
}
func (b KnowledgeBundle) ZIP() ([]byte, error) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	raw, e := json.MarshalIndent(b, "", "  ")
	if e != nil {
		return nil, e
	}
	f, e := z.Create("manifest.json")
	if e != nil {
		return nil, e
	}
	if _, e = f.Write(raw); e != nil {
		return nil, e
	}
	for _, p := range b.Pages {
		if _, e := uuid.Parse(p.ID); e != nil {
			return nil, ErrKnowledgeInvalid
		}
		f, e := z.Create(fmt.Sprintf("pages/%s/%d.md", p.ID, p.Revision))
		if e != nil {
			return nil, e
		}
		if _, e = fmt.Fprintf(f, "# %s\n\n%s\n\n## Sources\n\n%s\n", p.Title, p.Body, p.Sources); e != nil {
			return nil, e
		}
	}
	if e = z.Close(); e != nil {
		return nil, e
	}
	return buf.Bytes(), nil
}

func (s *Store) ImportKnowledge(ctx context.Context, a KnowledgeActor, team string, b KnowledgeBundle) (c KnowledgeCollection, err error) {
	if b.Version != 1 || !validKnowledgeTitle(b.Title) || len(b.Description) > 4000 || len(b.Pages) > KnowledgeMaxRevisions {
		return c, ErrKnowledgeInvalid
	}
	current := map[string]KnowledgePage{}
	ids := map[string]string{}
	seen := map[string]bool{}
	size := 0
	for _, p := range b.Pages {
		if !validKnowledgePage(p) || p.Revision < 1 {
			return c, ErrKnowledgeInvalid
		}
		key := fmt.Sprintf("%s/%d", p.ID, p.Revision)
		if seen[key] {
			return c, ErrKnowledgeInvalid
		}
		seen[key] = true
		size += len(p.Body) + len(p.Sources) + len(p.Title) + len(p.Reason)
		if size > KnowledgeMaxBytes {
			return c, ErrKnowledgeInvalid
		}
		if current[p.ID].Revision < p.Revision {
			current[p.ID] = p
		}
		ids[p.ID] = uuid.NewString()
	}
	if len(current) > KnowledgeMaxPages {
		return c, ErrKnowledgeInvalid
	}

	// Order parents once and reject cycles before creating any database state.
	var pages []KnowledgePage
	visiting, done := map[string]bool{}, map[string]bool{}
	size = 0
	var visit func(string) error
	visit = func(id string) error {
		if done[id] {
			return nil
		}
		p, ok := current[id]
		if !ok || visiting[id] {
			return ErrKnowledgeInvalid
		}
		visiting[id] = true
		if p.Parent != "" {
			if e := visit(p.Parent); e != nil {
				return e
			}
		}
		p.ID, p.Parent, p.Reason = ids[id], ids[p.Parent], "Imported snapshot"
		size += len(p.Body) + len(p.Sources) + len(p.Title) + len(p.Reason)
		if size > KnowledgeMaxBytes {
			return ErrKnowledgeInvalid
		}
		pages = append(pages, p)
		done[id] = true
		return nil
	}
	for id := range current {
		if err = visit(id); err != nil {
			return c, err
		}
	}
	c = KnowledgeCollection{ID: uuid.NewString(), TeamID: team, Title: b.Title, Description: b.Description, Revision: 1}
	if team == "" {
		c.OwnerUID = a.UID
	}
	access := func(tx *sql.Tx) error {
		if team == "" {
			return nil
		}
		t, e := teamFor(ctx, tx, a, team)
		if e != nil {
			return e
		}
		if t.Role != "admin" {
			return ErrNotFound
		}
		if !t.Writable() {
			return ErrKnowledgeReadOnly
		}
		return nil
	}
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		if e := access(tx); e != nil {
			return e
		}
		var count int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_collections WHERE owner_uid=? OR team_id=NULLIF(?,'')`, a.UID, team).Scan(&count); e != nil {
			return e
		}
		if count >= 100 {
			return ErrKnowledgeInvalid
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO knowledge_collections(id,owner_uid,team_id,title,description,archived,revision,created) VALUES(?,NULLIF(?,''),NULLIF(?,''),?,?,1,0,strftime('%s','now')*1000)`, c.ID, c.OwnerUID, team, c.Title, c.Description)
		return e
	})
	if err != nil {
		return c, err
	}
	// Revision zero prevents unarchiving partial content; remove it if any chunk fails.
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, e := s.db.ExecContext(cleanup, `DELETE FROM knowledge_collections WHERE id=?`, c.ID); e != nil {
				err = fmt.Errorf("%w; import cleanup: %v", err, e)
			}
		}
	}()
	const chunkSize = 16
	for start := 0; start < len(pages); start += chunkSize {
		err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
			if e := access(tx); e != nil {
				return e
			}
			for _, p := range pages[start:min(start+chunkSize, len(pages))] {
				p.CollectionID = c.ID
				if _, e := writeKnowledgePage(ctx, tx, a, p, 0); e != nil {
					return e
				}
			}
			return nil
		})
		if err != nil {
			return c, err
		}
	}
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		if e := access(tx); e != nil {
			return e
		}
		result, e := tx.ExecContext(ctx, `UPDATE knowledge_collections SET archived=0,revision=1 WHERE id=? AND revision=0`, c.ID)
		if e != nil {
			return e
		}
		count, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if count != 1 {
			return ErrNotFound
		}
		return nil
	})
	return
}

func (s *Store) PurgeKnowledgePage(ctx context.Context, a KnowledgeActor, collection, id string, expected int) error {
	return s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		c, e := collectionFor(ctx, tx, a, collection)
		if e != nil {
			return e
		}
		if c.Role != "admin" {
			return ErrNotFound
		}
		p, e := pageFor(ctx, tx, collection, id, 0)
		if e != nil {
			return e
		}
		if p.Revision != expected {
			return ErrKnowledgeConflict
		}
		if !p.Archived {
			return ErrKnowledgeReadOnly
		}
		var children int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_pages p JOIN knowledge_revisions r ON r.page_id=p.id AND r.revision=p.head WHERE p.collection_id=? AND r.parent=?`, collection, id).Scan(&children); e != nil {
			return e
		}
		if children > 0 {
			return ErrKnowledgeInvalid
		}
		if c.TeamID != "" {
			if e := auditTeam(ctx, tx, a, c.TeamID, "", "purge-page:"+id); e != nil {
				return e
			}
		}
		_, e = tx.ExecContext(ctx, `DELETE FROM knowledge_pages WHERE id=?`, id)
		return e
	})
}
