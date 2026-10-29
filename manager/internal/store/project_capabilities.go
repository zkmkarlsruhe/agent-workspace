// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"encoding/json"
)

func (s *Store) ProjectByID(ctx context.Context, uid, id string) (Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects p WHERE p.uid = ? AND p.id = ?`, uid, id))
}

func (s *Store) SetProjectCapabilities(ctx context.Context, uid, folder string, ids []string) error {
	if ids == nil {
		ids = []string{}
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	r, err := s.db.ExecContext(ctx, `UPDATE projects SET capabilities = ? WHERE uid = ? AND folder = ? AND profile_id = ''`, string(b), uid, folder)
	return changed(r, err)
}
