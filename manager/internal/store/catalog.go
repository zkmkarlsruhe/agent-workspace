// SPDX-License-Identifier: MIT

package store

import "context"

// OptIns returns the catalog ids a person switched on (portal Tools page).
func (s *Store) OptIns(ctx context.Context, uid string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT item FROM catalog_optins WHERE uid = ? ORDER BY item`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) SetOptIn(ctx context.Context, uid, item string, on bool) error {
	q := `DELETE FROM catalog_optins WHERE uid = ? AND item = ?`
	if on {
		q = `INSERT OR IGNORE INTO catalog_optins (uid, item) VALUES (?, ?)`
	}
	_, err := s.db.ExecContext(ctx, q, uid, item)
	return err
}
