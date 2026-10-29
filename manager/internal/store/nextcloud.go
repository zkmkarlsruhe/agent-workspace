// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// NCLink ties a project to its folder "Agent Workspace/<NCFolder>/" in the
// person's Nextcloud (internal/ncsync). Folder is the project folder now;
// NCFolder the name last created there, so a rename shows as a difference.
// The row keeps its project id through trash and restore; only projects in
// the projects table are listed.
type NCLink struct {
	ProjectID, UID, Folder, NCFolder string
	All                              bool // whole project, not only results/
	LinkedAt, LastSync               time.Time
	Note                             string // last conflict or error, for the portal
}

// NCFile is the state of one pushed or pulled file: project mtime (ms) and
// size at the transfer, and the ETag of the version in Nextcloud. Own is set
// only when that ETag came back from the manager's own successful PUT: only
// then may the next push overwrite it (D58); a pulled or pre-existing version
// is the person's and gets a "(Helfer)" copy.
type NCFile struct {
	Rel   string
	Size  int64
	MTime int64
	ETag  string
	Own   bool
}

const ncLinkCols = `l.project_id, l.uid, p.folder, l.nc_folder, l.sync_all, l.linked_at, l.last_sync, l.note`

func (s *Store) ncLinks(ctx context.Context, where string, args ...any) ([]NCLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ncLinkCols+` FROM nc_links l JOIN projects p ON p.id = l.project_id WHERE `+where+` ORDER BY p.folder`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NCLink
	for rows.Next() {
		var l NCLink
		var at, last int64
		if err := rows.Scan(&l.ProjectID, &l.UID, &l.Folder, &l.NCFolder, &l.All, &at, &last, &l.Note); err != nil {
			return nil, err
		}
		l.LinkedAt = time.UnixMilli(at)
		if last > 0 { // zero time = never transferred
			l.LastSync = time.UnixMilli(last)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// NCLinks lists the linked projects of uid.
func (s *Store) NCLinks(ctx context.Context, uid string) ([]NCLink, error) {
	return s.ncLinks(ctx, `l.uid = ?`, uid)
}

// NCLink returns the link of a project; ErrNotFound if it has none.
func (s *Store) NCLink(ctx context.Context, projectID string) (NCLink, error) {
	ls, err := s.ncLinks(ctx, `l.project_id = ?`, projectID)
	if err == nil && len(ls) == 0 {
		err = ErrNotFound
	}
	if err != nil {
		return NCLink{}, err
	}
	return ls[0], nil
}

// PutNCLink links a project or changes its scope; the file state survives.
func (s *Store) PutNCLink(ctx context.Context, projectID, uid, ncFolder string, syncAll bool, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO nc_links (project_id, uid, nc_folder, sync_all, linked_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (project_id) DO UPDATE SET sync_all = excluded.sync_all`, projectID, uid, ncFolder, syncAll, at.UnixMilli())
	return err
}

func (s *Store) DeleteNCLink(ctx context.Context, projectID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM nc_links WHERE project_id = ?`, projectID)
	return err
}

func (s *Store) SetNCFolder(ctx context.Context, projectID, ncFolder string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nc_links SET nc_folder = ? WHERE project_id = ?`, ncFolder, projectID)
	return err
}

// NCSynced records a transfer (at not zero) and the note for the portal.
func (s *Store) NCSynced(ctx context.Context, projectID string, at time.Time, note string) error {
	if at.IsZero() {
		_, err := s.db.ExecContext(ctx, `UPDATE nc_links SET note = ? WHERE project_id = ?`, note, projectID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE nc_links SET last_sync = ?, note = ? WHERE project_id = ?`, at.UnixMilli(), note, projectID)
	return err
}

func (s *Store) NCFiles(ctx context.Context, projectID string) (map[string]NCFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rel, size, mtime, etag, own FROM nc_files WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]NCFile{}
	for rows.Next() {
		var f NCFile
		if err := rows.Scan(&f.Rel, &f.Size, &f.MTime, &f.ETag, &f.Own); err != nil {
			return nil, err
		}
		out[f.Rel] = f
	}
	return out, rows.Err()
}

func (s *Store) PutNCFile(ctx context.Context, projectID string, f NCFile) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO nc_files (project_id, rel, size, mtime, etag, own) VALUES (?, ?, ?, ?, ?, ?)`,
		projectID, f.Rel, f.Size, f.MTime, f.ETag, f.Own)
	return err
}

// NCCollections lists the Nextcloud folders linked to a project as read sources (D47).
func (s *Store) NCCollections(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM nc_collections WHERE project_id = ? ORDER BY added_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddNCCollection returns ErrExists if the folder is already linked.
func (s *Store) AddNCCollection(ctx context.Context, projectID, uid, path string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO nc_collections (project_id, uid, path, added_at) VALUES (?, ?, ?, ?)`,
		projectID, uid, path, at.UnixMilli())
	if err != nil && isUnique(err) {
		return ErrExists
	}
	return err
}

func (s *Store) DeleteNCCollection(ctx context.Context, projectID, path string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM nc_collections WHERE project_id = ? AND path = ?`, projectID, path)
	return err
}

// SyncETag is the ETag of the version the manager itself put at path in
// the person's Nextcloud (ncsync.Upload), "" if none or unknown.
func (s *Store) SyncETag(ctx context.Context, uid, path string) (string, error) {
	var etag string
	err := s.db.QueryRowContext(ctx, `SELECT etag FROM nc_uploads WHERE uid = ? AND path = ?`, uid, path).Scan(&etag)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return etag, err
}

// PutSyncETag records the ETag the manager got back for its own write at path.
func (s *Store) PutSyncETag(ctx context.Context, uid, path, etag string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO nc_uploads (uid, path, etag) VALUES (?, ?, ?)`, uid, path, etag)
	return err
}
