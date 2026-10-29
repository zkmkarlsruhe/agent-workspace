// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"sync"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// WatchdogLoop owns watchers independently of optional Nextcloud sync. Each
// watcher uses the generation's leased client, never a reused slot's client.
func (s *Service) WatchdogLoop(ctx context.Context) {
	type worker struct {
		r      *running
		cancel context.CancelFunc
	}
	workers := map[string]worker{}
	var wg sync.WaitGroup
	defer func() {
		for _, w := range workers {
			w.cancel()
		}
		wg.Wait()
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		s.mu.Lock()
		for uid, w := range workers {
			if s.run[uid] != w.r {
				w.cancel()
				delete(workers, uid)
			}
		}
		for uid, r := range s.run {
			if _, ok := workers[uid]; !ok {
				wctx, cancel := context.WithCancel(ctx)
				workers[uid] = worker{r, cancel}
				wg.Go(func() { s.followWatchdog(wctx, uid, r) })
			}
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Service) followWatchdog(ctx context.Context, uid string, r *running) {
	ctx = logx.With(ctx, logx.Fields{UID: uid, Slot: r.slot})
	c := s.client(r)
	d := r.watch
	defer d.connection(false)
	// Keep HTTP snapshots off the SSE reader; deltas may arrive during a read.
	refresh := make(chan struct{}, 1)
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var reader sync.WaitGroup
	reader.Go(func() {
		for streamCtx.Err() == nil {
			err := c.Events(streamCtx, func(e ocapi.Event) {
				if e.Type == "server.connected" {
					d.connection(true)
					select {
					case refresh <- struct{}{}:
					default:
					}
					return
				}
				d.event(e, s.now())
			})
			d.connection(false)
			if streamCtx.Err() != nil {
				return
			}
			// Do not log upstream errors: a response body may contain chat content.
			s.log.DebugContext(ctx, "watchdog stream disconnected", "failed", err != nil)
			select {
			case <-streamCtx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	})
	defer func() { cancel(); reader.Wait() }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var lastRefresh time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh:
			s.refreshWatchdog(ctx, c, d)
			lastRefresh = s.now()
		case <-tick.C:
			if s.now().Sub(lastRefresh) >= 15*time.Second || d.needsMetadata() {
				s.refreshWatchdog(ctx, c, d)
				lastRefresh = s.now()
			}
		}
		s.watchdogNotices(ctx, uid, r)
	}
}

func (s *Service) refreshWatchdog(ctx context.Context, c *ocapi.Client, d *watchdog) bool {
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	defer cancel()
	version := d.version()
	active, err := c.Active(ctx)
	if err != nil || len(active) > 4096 {
		return false
	}
	var sessions []ocapi.Session
	waiting := map[string]bool{}
	dirs := map[string]bool{}
	for _, id := range active {
		se, err := c.SessionGet(ctx, id)
		if ocapi.IsStatus(err, 404) {
			continue
		}
		if err != nil || se.ID != id {
			return false
		}
		sessions = append(sessions, se)
		dirs[se.Location.Directory] = true
	}
	// shortcut: questions are sampled; add form/permission events if 15 s latency matters.
	for dir := range dirs {
		forms, err := c.Forms(ctx, dir)
		if err != nil {
			return false
		}
		perms, err := c.PermissionRequests(ctx, dir)
		if err != nil {
			return false
		}
		for _, raw := range append(forms, perms...) {
			var item struct {
				SessionID string `json:"sessionID"`
			}
			if json.Unmarshal(raw, &item) != nil {
				return false
			}
			waiting[item.SessionID] = true
			if item.SessionID == "global" || item.SessionID == "" {
				for _, se := range sessions {
					if se.Location.Directory == dir {
						waiting[se.ID] = true
					}
				}
			}
		}
	}
	d.reconcile(version, sessions, waiting, s.now())
	return true
}

func (s *Service) watchdogNotices(ctx context.Context, uid string, r *running) []WatchdogNotice {
	notices, detected := r.watch.snapshot(s.now())
	for _, n := range detected {
		if n.Retry {
			s.log.InfoContext(logx.With(ctx, logx.Fields{UID: uid, Slot: r.slot, SessionID: n.SessionID}),
				"watchdog retry detected", "op", "watchdog_retry", "attempt", n.Attempt, "next", n.Next)
			continue
		}
		s.log.InfoContext(logx.With(ctx, logx.Fields{UID: uid, Slot: r.slot, SessionID: n.SessionID}),
			"watchdog silence detected", "op", "watchdog_detect", "silent_s", n.SilentSeconds, "child", n.ParentID != "")
	}
	return notices
}

// Watchdog returns only this person's running generation, optionally one project.
func (s *Service) Watchdog(ctx context.Context, uid, project string) ([]WatchdogNotice, error) {
	if project != "" {
		if _, err := s.st.ProjectByFolder(ctx, uid, project); errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		} else if err != nil {
			return nil, err
		}
	}
	r := s.get(uid)
	if r == nil || r.watch == nil {
		return nil, nil
	}
	notices := s.watchdogNotices(ctx, uid, r)
	if project != "" {
		notices = slices.DeleteFunc(notices, func(n WatchdogNotice) bool { return n.Directory != s.projectDir(project) })
	}
	return notices, nil
}

// StopSilentSession is deliberately separate from stopping an environment.
func (s *Service) StopSilentSession(ctx context.Context, uid, project, id string) error {
	if id == "" || path.Base(id) != id || id == "." || id == ".." {
		return ErrNotFound
	}
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	r := s.get(uid)
	if r == nil {
		return ErrNotRunning
	}
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	defer cancel()
	// Recheck active membership and metadata against the same owner/generation.
	if !s.refreshWatchdog(ctx, s.client(r), r.watch) {
		return ErrUnavailable
	}
	notices, err := s.Watchdog(ctx, uid, project)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(notices, func(n WatchdogNotice) bool { return n.SessionID == id && n.SilentSeconds > 0 })
	if i < 0 {
		return ErrNotFound
	}
	err = s.client(r).SessionInterrupt(ctx, id)
	s.log.InfoContext(logx.With(ctx, logx.Fields{UID: uid, Slot: r.slot, SessionID: id}),
		"watchdog stop requested", "op", "watchdog_stop", "child", notices[i].ParentID != "", "success", err == nil)
	if err != nil {
		// Keep untrusted response text out of the portal's error logs.
		return errors.New("session interrupt failed")
	}
	// Acceptance is not settlement. Only terminal events or active reconciliation remove the notice.
	s.refreshWatchdog(ctx, s.client(r), r.watch)
	return nil
}
