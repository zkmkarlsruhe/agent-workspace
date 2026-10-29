// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
)

func TestReapParallelTicksEveryPerson(t *testing.T) {
	for _, parallel := range []int{1, 2} {
		t.Run(fmt.Sprint(parallel), func(t *testing.T) {
			e := setupInMemory(t)
			d := *instance.Current()
			d.Sessions.ArchiveAfterDays = 0
			defer instance.Select(&d)()
			e.s.cfg.ReapParallel, e.s.CollectChats = parallel, true
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			now := e.s.now()
			e.s.lastSweep = now
			var uids []string
			for i := range 3 {
				uid := fmt.Sprintf("%064x", i+1)
				u, err := e.st.EnsureUser(ctx, identity.Identity{UID: uid, Username: fmt.Sprintf("person%d", i)})
				if err != nil {
					t.Fatal(err)
				}
				uids = append(uids, uid)
				e.s.set(uid, &running{slot: u.Slot, started: now, lastRequest: now})
			}
			entered := make(chan struct{}, 3)
			release := make(chan struct{})
			var active, peak, calls atomic.Int32
			e.s.http.Transport = assistantTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/session/active" {
					t.Errorf("unexpected path %s", r.URL.Path)
					return
				}
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				calls.Add(1)
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
				}
				json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"busy": map[string]string{"type": "running"}}})
			})}
			done := make(chan struct{})
			go func() { e.s.Reap(ctx); close(done) }()
			for range parallel {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("workers did not start", ctx.Err())
				}
			}
			close(release)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("reap did not finish", ctx.Err())
			}
			if calls.Load() != 3 || peak.Load() != int32(parallel) {
				t.Fatalf("ticks %d, peak %d", calls.Load(), peak.Load())
			}
			for _, uid := range uids {
				if !e.s.get(uid).busySeen {
					t.Fatal("person missed tick", uid)
				}
			}
		})
	}
}
