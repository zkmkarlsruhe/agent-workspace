// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	KnowledgeMaxBody      = 128 << 10
	KnowledgeMaxSources   = 16 << 10
	KnowledgeMaxBytes     = 16 << 20
	KnowledgeMaxPages     = 1000
	KnowledgeMaxRevisions = 10000
)

type KnowledgeCollection struct {
	ID, OwnerUID, TeamID, TeamName, Title, Description, Role string
	Revision                                                 int
	Archived, Writable                                       bool
	Created                                                  int64
}
type KnowledgePage struct {
	ID, CollectionID                            string
	Revision                                    int
	Title, Parent, Body, Sources, Reason, Actor string
	Archived                                    bool
	Created                                     int64
}
type KnowledgeHit struct {
	CollectionID, CollectionTitle, PageID, Title, Snippet string
	Revision                                              int
}

const collectionAccess = `(c.owner_uid=? OR EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=c.team_id AND m.uid=? AND ` + acceptedTeamMember + `))`
const collectionColumns = `c.id,COALESCE(c.owner_uid,''),COALESCE(c.team_id,''),COALESCE(t.name,''),c.title,c.description,c.revision,c.archived,c.created`

func scanCollection(row scanner) (c KnowledgeCollection, err error) {
	err = row.Scan(&c.ID, &c.OwnerUID, &c.TeamID, &c.TeamName, &c.Title, &c.Description, &c.Revision, &c.Archived, &c.Created)
	return c, knowledgeError(err)
}
func collectionFor(ctx context.Context, tx *sql.Tx, a KnowledgeActor, id string) (c KnowledgeCollection, err error) {
	c, err = scanCollection(tx.QueryRowContext(ctx, `SELECT `+collectionColumns+` FROM knowledge_collections c LEFT JOIN teams t ON t.id=c.team_id WHERE c.id=? AND `+collectionAccess, id, a.UID, a.UID))
	if err != nil {
		return
	}
	if a.ProjectID != "" {
		var bound bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_knowledge WHERE project_id=? AND collection_id=?)`, a.ProjectID, id).Scan(&bound); err != nil {
			return
		}
		if !bound {
			return c, ErrNotFound
		}
	}
	c.Role = "admin"
	c.Writable = !c.Archived && c.Revision > 0
	if c.TeamID != "" {
		var t Team
		t, err = teamFor(ctx, tx, a, c.TeamID)
		c.Role = t.Role
		c.Writable = c.Writable && t.Writable()
	}
	return
}
func (s *Store) KnowledgeCollections(ctx context.Context, a KnowledgeActor) (out []KnowledgeCollection, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, `SELECT c.id FROM knowledge_collections c WHERE `+collectionAccess+` ORDER BY c.title,c.id`, a.UID, a.UID)
		if e != nil {
			return e
		}
		var ids []string
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, id := range ids {
			c, e := collectionFor(ctx, tx, a, id)
			if e != nil {
				return e
			}
			out = append(out, c)
		}
		return nil
	})
	return
}
func (s *Store) KnowledgeCollection(ctx context.Context, a KnowledgeActor, id string) (out KnowledgeCollection, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error { var e error; out, e = collectionFor(ctx, tx, a, id); return e })
	return
}
func (s *Store) CreateKnowledgeCollection(ctx context.Context, a KnowledgeActor, team, title, description string) (c KnowledgeCollection, err error) {
	if !validKnowledgeTitle(title) || len(description) > 4000 {
		return c, ErrKnowledgeInvalid
	}
	c = KnowledgeCollection{ID: uuid.NewString(), TeamID: team, Title: strings.TrimSpace(title), Description: description, Revision: 1, Created: time.Now().UnixMilli()}
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		if team != "" {
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
		} else {
			c.OwnerUID = a.UID
		}
		var n int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_collections WHERE owner_uid=? OR team_id=NULLIF(?,'')`, a.UID, team).Scan(&n); e != nil {
			return e
		}
		if n >= 100 {
			return ErrKnowledgeInvalid
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO knowledge_collections(id,owner_uid,team_id,title,description,created) VALUES(?,NULLIF(?,''),NULLIF(?,''),?,?,?)`, c.ID, c.OwnerUID, c.TeamID, c.Title, c.Description, c.Created)
		return e
	})
	return
}
func (s *Store) UpdateKnowledgeCollection(ctx context.Context, a KnowledgeActor, id, title, description string, archived bool, expected int) error {
	if !validKnowledgeTitle(title) || len(description) > 4000 {
		return ErrKnowledgeInvalid
	}
	return s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		c, e := collectionFor(ctx, tx, a, id)
		if e != nil {
			return e
		}
		if c.Role != "admin" {
			return ErrNotFound
		}
		if c.Revision != expected {
			return ErrKnowledgeConflict
		}
		if c.Revision == 0 {
			return ErrKnowledgeReadOnly
		}
		if c.TeamID != "" {
			t, e := teamFor(ctx, tx, a, c.TeamID)
			if e != nil {
				return e
			}
			if !t.Writable() {
				return ErrKnowledgeReadOnly
			}
		}
		_, e = tx.ExecContext(ctx, `UPDATE knowledge_collections SET title=?,description=?,archived=?,revision=revision+1 WHERE id=?`, strings.TrimSpace(title), description, archived, id)
		return e
	})
}
func (s *Store) PurgeKnowledgeCollection(ctx context.Context, a KnowledgeActor, id string, expected int) error {
	return s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		c, e := collectionFor(ctx, tx, a, id)
		if e != nil {
			return e
		}
		if c.Role != "admin" {
			return ErrNotFound
		}
		if c.Revision != expected {
			return ErrKnowledgeConflict
		}
		if !c.Archived {
			return ErrKnowledgeReadOnly
		}
		if c.TeamID != "" {
			if e := auditTeam(ctx, tx, a, c.TeamID, "", "purge-knowledge:"+id); e != nil {
				return e
			}
		}
		_, e = tx.ExecContext(ctx, `DELETE FROM knowledge_collections WHERE id=?`, id)
		return e
	})
}

const pageColumns = `p.id,p.collection_id,r.revision,r.title,r.parent,r.body,r.sources,r.reason,r.archived,COALESCE(r.actor,''),r.created`

func scanKnowledgePage(row scanner) (p KnowledgePage, err error) {
	err = row.Scan(&p.ID, &p.CollectionID, &p.Revision, &p.Title, &p.Parent, &p.Body, &p.Sources, &p.Reason, &p.Archived, &p.Actor, &p.Created)
	return p, knowledgeError(err)
}
func pageFor(ctx context.Context, tx *sql.Tx, collection, id string, revision int) (KnowledgePage, error) {
	return scanKnowledgePage(tx.QueryRowContext(ctx, `SELECT `+pageColumns+` FROM knowledge_pages p JOIN knowledge_revisions r ON r.page_id=p.id AND r.revision=CASE WHEN ?=0 THEN p.head ELSE ? END WHERE p.collection_id=? AND p.id=?`, revision, revision, collection, id))
}
func (s *Store) KnowledgePage(ctx context.Context, a KnowledgeActor, collection, id string, revision int) (p KnowledgePage, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		if _, e := collectionFor(ctx, tx, a, collection); e != nil {
			return e
		}
		var e error
		p, e = pageFor(ctx, tx, collection, id, revision)
		return e
	})
	return
}
func (s *Store) KnowledgePages(ctx context.Context, a KnowledgeActor, collection string) (out []KnowledgePage, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		if _, e := collectionFor(ctx, tx, a, collection); e != nil {
			return e
		}
		rows, e := tx.QueryContext(ctx, `SELECT p.id,p.collection_id,r.revision,r.title,r.parent,'','','',r.archived,COALESCE(r.actor,''),r.created FROM knowledge_pages p JOIN knowledge_revisions r ON r.page_id=p.id AND r.revision=p.head WHERE p.collection_id=? ORDER BY r.archived,r.title,p.id`, collection)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			p, e := scanKnowledgePage(rows)
			if e != nil {
				return e
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return
}
func (s *Store) KnowledgeHistory(ctx context.Context, a KnowledgeActor, collection, id string) (out []KnowledgePage, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		if _, e := collectionFor(ctx, tx, a, collection); e != nil {
			return e
		}
		if _, e := pageFor(ctx, tx, collection, id, 0); e != nil {
			return e
		}
		rows, e := tx.QueryContext(ctx, `SELECT p.id,p.collection_id,r.revision,r.title,r.parent,'','',r.reason,r.archived,COALESCE(r.actor,''),r.created FROM knowledge_pages p JOIN knowledge_revisions r ON r.page_id=p.id WHERE p.collection_id=? AND p.id=? ORDER BY r.revision DESC LIMIT 100`, collection, id)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			p, e := scanKnowledgePage(rows)
			if e != nil {
				return e
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return
}

func validKnowledgePage(p KnowledgePage) bool {
	parsed, err := uuid.Parse(p.ID)
	return err == nil && parsed.String() == p.ID && validKnowledgeTitle(p.Title) && len(p.Body) <= KnowledgeMaxBody && len(p.Sources) <= KnowledgeMaxSources && len(p.Reason) <= 1000 && utf8.ValidString(p.Body+p.Sources+p.Reason) && !strings.ContainsRune(p.Body+p.Sources+p.Reason, 0)
}
func saveKnowledgePage(ctx context.Context, tx *sql.Tx, a KnowledgeActor, p KnowledgePage, expected int) (KnowledgePage, error) {
	if !validKnowledgePage(p) || expected < 0 {
		return p, ErrKnowledgeInvalid
	}
	c, e := collectionFor(ctx, tx, a, p.CollectionID)
	if e != nil {
		return p, e
	}
	if !c.Writable {
		return p, ErrKnowledgeReadOnly
	}
	old, e := pageFor(ctx, tx, p.CollectionID, p.ID, 0)
	if e != nil && !errors.Is(e, ErrNotFound) {
		return p, e
	}
	if e == nil && expected == 0 && old.Revision == 1 && old.Title == p.Title && old.Body == p.Body && old.Sources == p.Sources && old.Parent == p.Parent && old.Reason == p.Reason && old.Archived == p.Archived && old.Actor == a.UID {
		return old, nil
	}
	if old.Revision != expected {
		return p, ErrKnowledgeConflict
	}
	if p.Parent != "" {
		visited := map[string]bool{p.ID: true}
		next := p.Parent
		for next != "" {
			if visited[next] {
				return p, ErrKnowledgeInvalid
			}
			visited[next] = true
			parent, e := pageFor(ctx, tx, p.CollectionID, next, 0)
			if e != nil {
				return p, ErrKnowledgeInvalid
			}
			next = parent.Parent
		}
	}
	var count, bytes, revisions int
	if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM knowledge_pages WHERE collection_id=?`, p.CollectionID).Scan(&count); e != nil {
		return p, e
	}
	if count >= KnowledgeMaxPages && expected == 0 {
		return p, ErrKnowledgeInvalid
	}
	if e := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(CAST(r.body||r.sources||r.title||r.reason AS BLOB))),0) FROM knowledge_revisions r JOIN knowledge_pages p ON p.id=r.page_id WHERE p.collection_id=?`, p.CollectionID).Scan(&revisions, &bytes); e != nil {
		return p, e
	}
	if revisions >= KnowledgeMaxRevisions || bytes+len(p.Body)+len(p.Sources)+len(p.Title)+len(p.Reason) > KnowledgeMaxBytes {
		return p, ErrKnowledgeInvalid
	}
	return writeKnowledgePage(ctx, tx, a, p, expected)
}

// writeKnowledgePage persists a page after access, tree and quota validation.
func writeKnowledgePage(ctx context.Context, tx *sql.Tx, a KnowledgeActor, p KnowledgePage, expected int) (KnowledgePage, error) {
	if expected == 0 {
		if _, e := tx.ExecContext(ctx, `INSERT INTO knowledge_pages(id,collection_id) VALUES(?,?)`, p.ID, p.CollectionID); e != nil {
			if isUnique(e) {
				return p, ErrKnowledgeInvalid
			}
			return p, e
		}
	}
	p.Revision = expected + 1
	p.Actor = a.UID
	p.Created = time.Now().UnixMilli()
	if _, e := tx.ExecContext(ctx, `INSERT INTO knowledge_revisions VALUES(?,?,?,?,?,?,?,?,?,?)`, p.ID, p.Revision, p.Title, p.Parent, p.Body, p.Sources, p.Reason, p.Archived, p.Actor, p.Created); e != nil {
		return p, e
	}
	if _, e := tx.ExecContext(ctx, `UPDATE knowledge_pages SET head=? WHERE id=?`, p.Revision, p.ID); e != nil {
		return p, e
	}
	if _, e := tx.ExecContext(ctx, `DELETE FROM knowledge_search WHERE page_id=?`, p.ID); e != nil {
		return p, e
	}
	if !p.Archived {
		if _, e := tx.ExecContext(ctx, `INSERT INTO knowledge_search VALUES(?,?,?)`, p.ID, p.Title, p.Body); e != nil {
			return p, e
		}
	}
	return p, nil
}
func (s *Store) SaveKnowledgePage(ctx context.Context, a KnowledgeActor, p KnowledgePage, expected int) (out KnowledgePage, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error { var e error; out, e = saveKnowledgePage(ctx, tx, a, p, expected); return e })
	return
}
func (s *Store) RestoreKnowledgeRevision(ctx context.Context, a KnowledgeActor, collection, id string, revision, expected int) (out KnowledgePage, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		if _, e := collectionFor(ctx, tx, a, collection); e != nil {
			return e
		}
		p, e := pageFor(ctx, tx, collection, id, revision)
		if e != nil {
			return e
		}
		p.Archived = false
		p.Reason = "Restore revision"
		out, e = saveKnowledgePage(ctx, tx, a, p, expected)
		return e
	})
	return
}

func (s *Store) SearchKnowledge(ctx context.Context, a KnowledgeActor, collection, query string) (out []KnowledgeHit, err error) {
	words := strings.Fields(query)
	if len(words) == 0 {
		return out, nil
	}
	if len(query) > 300 || len(words) > 20 {
		return out, ErrKnowledgeInvalid
	}
	for i, w := range words {
		words[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		if collection != "" {
			if _, e := collectionFor(ctx, tx, a, collection); e != nil {
				return e
			}
		}
		rows, e := tx.QueryContext(ctx, `SELECT c.id,c.title,p.id,r.title,snippet(knowledge_search,2,'[',']',' … ',24),p.head
   FROM knowledge_search JOIN knowledge_pages p ON p.id=knowledge_search.page_id
   JOIN knowledge_collections c ON c.id=p.collection_id JOIN knowledge_revisions r ON r.page_id=p.id AND r.revision=p.head
   LEFT JOIN teams t ON t.id=c.team_id
   WHERE knowledge_search MATCH ? AND c.archived=0 AND r.archived=0 AND COALESCE(t.archived,0)=0 AND `+collectionAccess+`
   AND (?='' OR c.id=?) AND (?='' OR EXISTS(SELECT 1 FROM project_knowledge b WHERE b.collection_id=c.id AND b.project_id=?)) ORDER BY lower(r.title),p.id LIMIT 50`, strings.Join(words, " AND "), a.UID, a.UID, collection, collection, a.ProjectID, a.ProjectID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var h KnowledgeHit
			if e = rows.Scan(&h.CollectionID, &h.CollectionTitle, &h.PageID, &h.Title, &h.Snippet, &h.Revision); e != nil {
				return e
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	return
}

func (s *Store) BindKnowledge(ctx context.Context, a KnowledgeActor, project, collection string, on bool) error {
	return s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		var owned bool
		if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=? AND uid=? AND profile_id='')`, project, a.UID).Scan(&owned); e != nil {
			return e
		}
		if !owned {
			return ErrNotFound
		}
		if on {
			c, e := collectionFor(ctx, tx, a, collection)
			if e != nil {
				return e
			}
			if c.Archived {
				return ErrKnowledgeReadOnly
			}
			_, e = tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_knowledge VALUES(?,?)`, project, collection)
			return e
		}
		_, e := tx.ExecContext(ctx, `DELETE FROM project_knowledge WHERE project_id=? AND collection_id=?`, project, collection)
		return e
	})
}
func (s *Store) ProjectKnowledge(ctx context.Context, a KnowledgeActor, project string) (out []KnowledgeCollection, err error) {
	err = s.knowledgeTx(ctx, a, func(tx *sql.Tx) error {
		var owned bool
		if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=? AND uid=? AND profile_id='')`, project, a.UID).Scan(&owned); e != nil {
			return e
		}
		if !owned {
			return ErrNotFound
		}
		rows, e := tx.QueryContext(ctx, `SELECT c.id FROM project_knowledge b JOIN knowledge_collections c ON c.id=b.collection_id WHERE b.project_id=? AND `+collectionAccess+` ORDER BY c.title`, project, a.UID, a.UID)
		if e != nil {
			return e
		}
		var ids []string
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, id := range ids {
			c, e := collectionFor(ctx, tx, a, id)
			if e != nil {
				return e
			}
			out = append(out, c)
		}
		return nil
	})
	return
}
