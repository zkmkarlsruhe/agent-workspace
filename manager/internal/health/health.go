// SPDX-License-Identifier: MIT

// Package health serves GET /health for the external Gatus check (BLUEPRINT §11 #13).
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"
)

type Handler struct {
	DiskPath string     // filesystem holding the DB (and user data on the VM)
	Running  func() int // containers tracked as running
	// Checks are the dependencies by name (store, engine, provision, pdf,
	// catalog); an error makes ok false unless the name is in Optional.
	Checks map[string]func(ctx context.Context) error
	// Optional names checks of optional integrations (pdf, catalog): reported
	// in checks, but a failure leaves ok true, so Gatus does not alarm on them.
	Optional map[string]bool

	mu   sync.Mutex
	at   time.Time
	last report
}

type report struct {
	OK                bool              `json:"ok"`
	DiskFreeBytes     uint64            `json:"disk_free_bytes"`
	DiskFreePct       float64           `json:"disk_free_pct"`
	LastBackupAgeSecs *float64          `json:"last_backup_age_seconds"` // shortcut: always null until the backup job writes a marker
	Running           int               `json:"running_containers"`
	Checks            map[string]string `json:"checks"`             // "ok" or the error
	Optional          []string          `json:"optional,omitempty"` // checks that do not count for ok
}

const (
	minFreePct = 5 // matches the start refusal threshold of docs/adr/README.md §6
	cacheFor   = 15 * time.Second
	budget     = 800 * time.Millisecond // all checks run in parallel; Gatus wants < 1 s
)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock() // also single-flights the checks
	if time.Since(h.at) >= cacheFor {
		h.last, h.at = h.check(r.Context()), time.Now()
	}
	rep := h.last
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !rep.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(rep)
}

func (h *Handler) check(ctx context.Context) report {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	rep := report{Running: h.Running(), Checks: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, f := range h.Checks {
		wg.Go(func() {
			res := "ok"
			if err := f(ctx); err != nil {
				res = err.Error()
			}
			mu.Lock()
			rep.Checks[name] = res
			mu.Unlock()
		})
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(h.DiskPath, &st); err == nil && st.Blocks > 0 {
		rep.DiskFreeBytes = st.Bavail * uint64(st.Bsize)
		rep.DiskFreePct = 100 * float64(st.Bavail) / float64(st.Blocks)
	}
	wg.Wait()
	rep.OK = rep.DiskFreePct >= minFreePct
	for name, res := range rep.Checks {
		if h.Optional[name] {
			rep.Optional = append(rep.Optional, name)
			continue
		}
		rep.OK = rep.OK && res == "ok"
	}
	slices.Sort(rep.Optional)
	return rep
}

// HTTPOK checks that GET url answers 200 (Gotenberg /health).
func HTTPOK(c *http.Client, url string) func(context.Context) error {
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", url, resp.Status)
		}
		return nil
	}
}

// DirReadable checks that dir can be listed (the catalog after an rsync).
func DirReadable(dir string) func(context.Context) error {
	return func(context.Context) error {
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err = f.Readdirnames(1); err == io.EOF {
			return nil // empty, but readable
		}
		return err
	}
}
