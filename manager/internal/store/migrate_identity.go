// SPDX-License-Identifier: MIT

package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

func migrateIdentity(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT username FROM users ORDER BY username`)
	if err != nil {
		return err
	}
	var names, duplicates []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		// EqualFold also checks Unicode names; SQLite NOCASE only folds ASCII.
		for _, seen := range names {
			if strings.EqualFold(seen, name) {
				duplicates = append(duplicates, seen, name)
			}
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(duplicates) != 0 {
		slices.Sort(duplicates)
		return fmt.Errorf("duplicate usernames (case-insensitive): %q; resolve these accounts before retrying migration 21", slices.Compact(duplicates))
	}
	_, err = tx.Exec(identityMigrationSQL)
	return err
}

const identityMigrationSQL = `
CREATE UNIQUE INDEX users_username ON users (username);
ALTER TABLE users ADD COLUMN identity_issuer TEXT;
ALTER TABLE users ADD COLUMN identity_subject TEXT;
ALTER TABLE users ADD COLUMN blocked_issuer TEXT;
ALTER TABLE users ADD COLUMN blocked_subject TEXT;
CREATE TABLE identity_releases (
	ts INTEGER NOT NULL,
	uid TEXT NOT NULL REFERENCES users(uid) ON DELETE CASCADE,
	admin_uid TEXT NOT NULL,
	old_subject_hash TEXT NOT NULL,
	new_subject_hash TEXT NOT NULL
);
`
