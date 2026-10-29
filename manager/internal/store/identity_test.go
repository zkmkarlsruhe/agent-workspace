// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
)

func TestIdentityBinding(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id := identity.Identity{UID: uid(1), Username: "alice", Issuer: "https://id.example/", Subject: "person-1", Email: "old@example.org"}
	if got, err := s.CheckIdentity(ctx, id); err != nil || got != id.UID {
		t.Fatalf("first check: %q %v", got, err)
	}
	if _, err := s.UserByUID(ctx, id.UID); err != ErrNotFound {
		t.Fatalf("account created before consent: %v", err)
	}
	original, err := s.EnsureUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.CheckIdentity(ctx, id); err != nil || got != original.UID {
		t.Fatalf("normal login: %q %v", got, err)
	}
	unbound := id
	unbound.Issuer, unbound.Subject = "", ""
	if _, err := s.EnsureUser(ctx, unbound); err != identity.ErrInvalid {
		t.Fatalf("bound account accepted absent subject: %v", err)
	}
	incoming := id
	incoming.UID, incoming.Subject, incoming.Email = uid(2), "person-2", "new@example.org"
	if _, err := s.EnsureUser(ctx, incoming); !errors.Is(err, ErrIdentityBlocked) {
		t.Fatalf("mismatch accepted: %v", err)
	}
	// Neither the original subject nor a third candidate can bypass or replace the block.
	third := incoming
	third.Subject = "person-3"
	for _, candidate := range []identity.Identity{id, incoming, third} {
		if _, err := s.CheckIdentity(ctx, candidate); !errors.Is(err, ErrIdentityBlocked) {
			t.Fatalf("block not latched: %v", err)
		}
	}
	blocks, err := s.IdentityBlocks(ctx)
	if err != nil || len(blocks) != 1 || blocks[0].PendingHash != identity.SubjectHash(incoming.Issuer, incoming.Subject) {
		t.Fatalf("pending: %+v %v", blocks, err)
	}
	if got, err := s.UserByUID(ctx, original.UID); err != nil || got != original {
		t.Fatalf("blocked request changed account: %+v %v", got, err)
	}
	if err := s.ReleaseIdentity(ctx, original.UID, "stale", uid(9)); err != ErrNotFound {
		t.Fatalf("stale form: %v", err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_release BEFORE INSERT ON identity_releases BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseIdentity(ctx, original.UID, blocks[0].PendingHash, uid(9)); err == nil {
		t.Fatal("release succeeded without an audit")
	}
	if _, err := s.CheckIdentity(ctx, incoming); !errors.Is(err, ErrIdentityBlocked) {
		t.Fatalf("failed audit cleared block: %v", err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER refuse_release`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseIdentity(ctx, original.UID, blocks[0].PendingHash, uid(9)); err != nil {
		t.Fatal(err)
	}
	var actor, oldHash, newHash string
	var count int
	if err := s.db.QueryRow(`SELECT admin_uid, old_subject_hash, new_subject_hash FROM identity_releases WHERE uid = ?`, original.UID).Scan(&actor, &oldHash, &newHash); err != nil {
		t.Fatal(err)
	}
	if actor != uid(9) || oldHash != blocks[0].StoredHash || newHash != blocks[0].PendingHash {
		t.Fatal("release audit does not match")
	}
	if err := s.ReleaseIdentity(ctx, original.UID, blocks[0].PendingHash, uid(9)); err != ErrNotFound {
		t.Fatalf("replayed release: %v", err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM identity_releases`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("release count: %d %v", count, err)
	}
	got, err := s.EnsureUser(ctx, incoming)
	if err != nil || got.UID != original.UID || got.Slug != original.Slug || got.Slot != original.Slot || got.Email != incoming.Email {
		t.Fatalf("rebind changed ownership key: %+v %v", got, err)
	}
	incoming.Issuer = "https://other.example/"
	if _, err := s.CheckIdentity(ctx, incoming); !errors.Is(err, ErrIdentityIssuerMismatch) {
		t.Fatalf("issuer mismatch accepted: %v", err)
	}
	if blocks, err := s.IdentityBlocks(ctx); err != nil || len(blocks) != 0 {
		t.Fatalf("issuer mismatch created a subject block: %+v %v", blocks, err)
	}
}

func TestConcurrentIdentityFirstBind(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 1; i <= 2; i++ {
		wg.Go(func() {
			_, err := s.EnsureUser(ctx, identity.Identity{UID: uid(i), Username: "same", Issuer: "issuer", Subject: uid(i)})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	bound, blocked := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			bound++
		case errors.Is(err, ErrIdentityBlocked):
			blocked++
		default:
			t.Fatal(err)
		}
	}
	if bound != 1 || blocked != 1 {
		t.Fatalf("bound %d blocked %d", bound, blocked)
	}
}

func legacyIdentityDB(t *testing.T, duplicate bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, q := range migrations[:20] {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if q != "" {
			if _, err := tx.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
		if fn := migrationFuncs[i+1]; fn != nil {
			if err := fn(tx); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 20`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (uid, username, slug, slot, created_at) VALUES (?, 'alice', 'alice', 1, 0)`, uid(1)); err != nil {
		t.Fatal(err)
	}
	if duplicate {
		if _, err := db.Exec(`INSERT INTO users (uid, username, slug, slot, created_at) VALUES (?, 'alice', 'alice-2', 2, 0)`, uid(2)); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestIdentityMigration(t *testing.T) {
	s, err := Open(legacyIdentityDB(t, false))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var issuer, subject sql.NullString
	if err := s.db.QueryRow(`SELECT identity_issuer, identity_subject FROM users`).Scan(&issuer, &subject); err != nil || issuer.Valid || subject.Valid {
		t.Fatalf("migration must leave legacy unbound: %v", err)
	}
	id := identity.Identity{UID: uid(2), Username: "alice", Issuer: "issuer", Subject: "stable"}
	if got, err := s.CheckIdentity(context.Background(), id); err != nil || got != uid(1) {
		t.Fatalf("legacy bind must keep UID: %q %v", got, err)
	}
	if err := s.db.QueryRow(`SELECT identity_issuer, identity_subject FROM users`).Scan(&issuer, &subject); err != nil || issuer.String != id.Issuer || subject.String != id.Subject {
		t.Fatalf("legacy bind not persisted: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO users (uid, username, slug, slot, created_at) VALUES (?, 'alice', 'other', 2, 0)`, uid(3)); err == nil {
		t.Fatal("username unique index missing")
	}
	if duplicate, err := Open(legacyIdentityDB(t, true)); err == nil {
		duplicate.Close()
		t.Fatal("ambiguous legacy usernames must fail migration")
	}
}

func TestIdentityIssuerStartupCheck(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.CheckIdentityIssuer(ctx, "authentik"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []identity.Identity{
		{UID: uid(1), Username: "legacy"},
		{UID: uid(2), Username: "bound", Issuer: "authentik", Subject: "person-2"},
		{UID: uid(3), Username: "other", Issuer: "other-idp", Subject: "person-3"},
	} {
		if _, err := s.EnsureUser(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// Pending bindings from the previous implementation must also be migrated.
	if _, err := s.db.Exec(`UPDATE users SET blocked_issuer = 'previous-idp', blocked_subject = 'pending' WHERE uid = ?`, uid(2)); err != nil {
		t.Fatal(err)
	}
	for issuer, count := range map[string]string{"authentik": "2 account(s)", "other-idp": "1 account(s)"} {
		err := s.CheckIdentityIssuer(ctx, issuer)
		if !errors.Is(err, ErrIdentityIssuerMismatch) || !strings.Contains(err.Error(), count) {
			t.Fatalf("issuer %q: %v", issuer, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE users SET identity_issuer = 'authentik', blocked_issuer = CASE WHEN blocked_issuer IS NOT NULL THEN 'authentik' END WHERE identity_issuer IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIdentityIssuer(ctx, "authentik"); err != nil {
		t.Fatal(err)
	}
	var subject sql.NullString
	if err := s.db.QueryRow(`SELECT identity_subject FROM users WHERE uid = ?`, uid(1)).Scan(&subject); err != nil || subject.Valid {
		t.Fatalf("startup check bound a legacy account: %v", err)
	}
	for _, issuer := range []string{"", " ", "bad\x00issuer"} {
		if err := s.CheckIdentityIssuer(ctx, issuer); err == nil {
			t.Fatalf("invalid configured issuer accepted: %q", issuer)
		}
	}
}

func TestIdentityMigrationDuplicatePreflight(t *testing.T) {
	for _, names := range [][]string{{"alice", "alice"}, {"alice", "Alice"}, {"Jürgen", "JÜRGEN"}, {"Σ", "ς"}} {
		t.Run(strings.Join(names, "/"), func(t *testing.T) {
			path := legacyIdentityDB(t, false)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE users SET username = ?`, names[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO users (uid, username, slug, slot, created_at) VALUES (?, ?, 'other', 2, 0)`, uid(2), names[1]); err != nil {
				t.Fatal(err)
			}
			if st, err := Open(path); err == nil {
				st.Close()
				t.Fatal("duplicate usernames accepted")
			} else {
				for _, part := range append([]string{"migration 21", "duplicate usernames (case-insensitive)"}, names...) {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("diagnostic lacks %q: %v", part, err)
					}
				}
			}
			var version, count, schemaChanges int
			if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM users WHERE username IN (?, ?)`, names[0], names[1]).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('users_username', 'identity_releases')`).Scan(&schemaChanges); err != nil {
				t.Fatal(err)
			}
			if version != 20 || count != 2 || schemaChanges != 0 {
				t.Fatalf("preflight changed database: version=%d rows=%d schema=%d", version, count, schemaChanges)
			}
			if _, err := db.Exec(`UPDATE users SET username = 'resolved' WHERE uid = ?`, uid(2)); err != nil {
				t.Fatal(err)
			}
			st, err := Open(path)
			if err != nil {
				t.Fatalf("migration after resolving duplicate: %v", err)
			}
			st.Close()
		})
	}
}

func TestDerivedUIDPreservesStoredAccount(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	original, err := s.EnsureUser(ctx, identity.Identity{UID: uid(1), Username: "alice", Issuer: "oidc", Subject: "person-42"})
	if err != nil {
		t.Fatal(err)
	}
	c := identity.Checker{Trusted: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, Issuer: "oidc", Headers: identity.Headers{UID: "-"}}
	r := httptest.NewRequest("GET", "https://workspace.example/", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("X-Authentik-Uid", "person-42")
	r.Header.Set("X-Authentik-Username", "alice")
	id, err := c.FromRequest(r)
	if err != nil || id.UID == original.UID {
		t.Fatalf("expected derived candidate: %+v %v", id, err)
	}
	if got, err := s.CheckIdentity(ctx, id); err != nil || got != original.UID {
		t.Fatalf("pre-consent changed UID: %s %v", got, err)
	}
	got, err := s.EnsureUser(ctx, id)
	if err != nil || got != original {
		t.Fatalf("stored account changed: %+v %v", got, err)
	}
}
