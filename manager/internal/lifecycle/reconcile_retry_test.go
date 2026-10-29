// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
)

func TestReconcileContinuesAfterRemoveFailure(t *testing.T) {
	e := setupInMemory(t)
	ctx := context.Background()
	u, err := e.st.EnsureUser(ctx, identity.Identity{UID: uid2, Username: "other"})
	if err != nil {
		t.Fatal(err)
	}
	for slot, uid := range map[int]string{1: uid1, u.Slot: uid2} {
		name := config.SlotContainer(slot)
		e.rt.Ctrs[name] = &runtime.Container{ID: name, Name: name, Running: true,
			Labels: map[string]string{"oc.uid": uid, "oc.slot": strconv.Itoa(slot), "oc.generation": "5", runtime.LabelManaged: "1"}}
	}
	e.rt.Ctrs["oc-user9"] = &runtime.Container{ID: "orphan", Name: "oc-user9"}
	e.rt.RemoveErr = map[string]error{"oc-user9": errors.New("engine unavailable")}
	if err := e.s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if e.s.get(uid1) == nil || e.s.get(uid2) == nil || e.rt.Ctrs["oc-user9"] == nil {
		t.Fatal("remove failure prevented adoption or removed the failed container")
	}
}

func TestReapRetriesAdoption(t *testing.T) {
	for _, initial := range []string{"reconcile", "start"} {
		t.Run(initial, func(t *testing.T) {
			e := setupInMemory(t)
			ctx := context.Background()
			e.rt.Ctrs["oc-user1"] = &runtime.Container{ID: "existing", Name: "oc-user1", Running: true,
				Labels: map[string]string{"oc.uid": uid1, "oc.slot": "1", "oc.generation": "5", runtime.LabelManaged: "1"}}
			probe := &probeTransport{RoundTripper: assistantTransport{e.oc}, fail: true}
			e.s.http = &http.Client{Transport: probe}
			if initial == "reconcile" {
				if err := e.s.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
			} else if _, err := e.s.EnsureRunning(ctx, uid1); err == nil {
				t.Fatal("failed probe accepted")
			}
			if e.s.get(uid1) != nil || len(e.rt.Removed) != 0 || probe.probes != 1 {
				t.Fatal("inconclusive probe must retain the untracked container")
			}
			probe.fail = false
			lock := e.s.lock(uid1)
			lock.Lock()
			e.s.Reap(ctx)
			lock.Unlock()
			if e.s.get(uid1) != nil {
				t.Fatal("adopted while person lock held")
			}
			e.s.Reap(ctx)
			r := e.s.get(uid1)
			if r == nil || r.gen != 5 || len(e.rt.Specs) != 0 || len(e.rt.Removed) != 0 {
				t.Fatal("next reap must adopt the existing container", r)
			}
		})
	}
}

func TestReapAdoptionProbeOutcomes(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			e := setupInMemory(t)
			e.rt.Ctrs["oc-user1"] = &runtime.Container{ID: "existing", Name: "oc-user1", Running: true,
				Labels: map[string]string{"oc.uid": uid1, "oc.slot": "1", "oc.generation": "5", runtime.LabelManaged: "1"}}
			e.s.http.Transport = assistantTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/info" {
					t.Errorf("unexpected probe %s", r.URL.Path)
				}
				w.WriteHeader(status)
				w.Write([]byte(`{"version":"2.2.0"}`))
			})}
			e.s.Reap(context.Background())
			adopted := status == http.StatusOK || status == http.StatusServiceUnavailable
			removed := status == http.StatusUnauthorized
			if (e.s.get(uid1) != nil) != adopted || (e.rt.Ctrs["oc-user1"] == nil) != removed {
				t.Fatalf("status %d: tracked %v, removed %v", status, e.s.get(uid1), e.rt.Removed)
			}
		})
	}
}
