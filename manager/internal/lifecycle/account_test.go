// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

func TestDeleteAccount(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.s.DeleteAccount(ctx, uid1); err != ErrUnavailable {
		t.Fatalf("without oc-provision: %v", err)
	}
	var purged []string
	fail := errors.New("helper down")
	e.s.PurgeData = func(_ context.Context, uid string, slot int) error {
		if fail != nil {
			return fail
		}
		purged = append(purged, uid)
		return nil
	}
	var provisioned []int
	e.s.Provision = func(_ context.Context, _ string, slot int) error { provisioned = append(provisioned, slot); return nil }
	for _, n := range []string{"a", "b"} {
		if err := e.s.CreateProject(ctx, uid1, NewProject{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.s.MoveToTrash(ctx, uid1, "b"); err != nil {
		t.Fatal(err)
	}
	pa, _ := e.st.ProjectByFolder(ctx, uid1, "a")
	eur := 1.25
	e.st.InsertUsage(ctx, store.UsageEvent{EventID: "u1", Start: time.Now(), End: time.Now(), UID: uid1, ProjectID: pa.ID,
		Capability: "llm", Status: 200, CostEUR: &eur})

	// a failing purge keeps the account for a retry
	if _, err := e.s.DeleteAccount(ctx, uid1); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	if _, err := e.st.UserByUID(ctx, uid1); err != nil {
		t.Fatal("rows went before the data")
	}
	fail = nil
	// a stored Nextcloud login without a configured house service blocks the
	// deletion: the cascade would drop it while the app password stays valid
	if err := e.st.PutConnection(ctx, store.Connection{UID: uid1, Service: "nextcloud", Login: "alice", SecretEnc: []byte{1}, ConnectedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.DeleteAccount(ctx, uid1); !errors.Is(err, ErrUnavailable) || len(purged) != 0 {
		t.Fatalf("%v, purged %v", err, purged)
	}
	if _, err := e.st.UserByUID(ctx, uid1); err != nil {
		t.Fatal("account gone with an unrevoked login")
	}
	// a house service that cannot revoke keeps the account, before anything is gone
	var disconnected []string
	ncDown := errors.New("nextcloud down")
	e.s.Disconnect = func(ctx context.Context, uid string) error {
		disconnected = append(disconnected, uid)
		if ncDown != nil {
			return ncDown
		}
		return e.st.DeleteConnection(ctx, uid, "nextcloud")
	}
	if _, err := e.s.DeleteAccount(ctx, uid1); !errors.Is(err, ncDown) || len(purged) != 0 {
		t.Fatalf("%v, purged %v", err, purged)
	}
	ncDown = nil
	if _, err := e.s.DeleteAccount(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(purged, []string{uid1}) || !slices.Equal(disconnected, []string{uid1, uid1}) {
		t.Fatalf("purged %v", purged)
	}
	if e.s.get(uid1) != nil || len(e.rt.Ctrs) != 0 || !slices.Contains(e.rt.NetsGone, "oc-u1") {
		t.Fatalf("container or network left: %v %v", e.rt.Ctrs, e.rt.NetsGone)
	}
	if _, err := e.st.UserByUID(ctx, uid1); err != store.ErrNotFound {
		t.Fatal("user row left")
	}
	if ps, _ := e.st.Projects(ctx, uid1); len(ps) != 0 {
		t.Fatal("projects left")
	}
	if ts, _ := e.st.Trash(ctx, uid1); len(ts) != 0 {
		t.Fatal("trash left")
	}
	if tot, _ := e.st.UsageTotal(ctx, uid1, time.Time{}); tot.CostEUR != 0 {
		t.Fatal("usage still under the uid")
	}
	if _, err := e.s.DeleteAccount(ctx, uid1); err != store.ErrNotFound {
		t.Fatalf("second delete: %v", err)
	}

	// next visit: a fresh account on the free slot, provisioned on the first start
	u, err := e.st.EnsureUser(ctx, identity.Identity{UID: uid1, Username: "alice"})
	if err != nil || u.Slot != 1 {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := e.s.EnsureRunning(ctx, uid1); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(provisioned, []int{1, 1}) {
		t.Fatalf("provisioned %v", provisioned)
	}
	if ps, _ := e.s.ListProjects(ctx, uid1); len(ps) != 0 {
		t.Fatal("fresh account has projects")
	}
}
