// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/logx"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/runtime"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

type action int

func (a action) String() string {
	return [...]string{"keep", "stop if idle", "begin drain", "drain or kill"}[a]
}

const (
	keep        action = iota
	stopIfIdle         // stop only if GET /api/session/active is empty
	beginDrain         // MaxLifetime reached
	drainOrKill        // draining: stop when idle; after DrainCap interrupt, then stop
)

// decide is the idle algorithm of docs/adr/README.md §3.11 without the network part.
// shortcut: no wait_cap for open permission/form questions (needs the event
// subscription); a waiting agent counts as running and keeps the container.
func decide(now time.Time, r running, c config.Config) action {
	switch {
	case r.draining:
		return drainOrKill
	case now.Sub(r.started) > c.MaxLifetime:
		return beginDrain
	case r.streams > 0 && now.Sub(r.lastRequest) < c.TabCap:
		return keep
	case now.Sub(r.lastRequest) < c.IdleTimeout:
		return keep
	}
	return stopIfIdle
}

// Reap runs one idle tick over all running containers.
func (s *Service) Reap(ctx context.Context) {
	ctx, done, ok := s.beginWork(ctx)
	if !ok {
		return
	}
	defer done()
	s.mu.Lock()
	snap := make(map[string]running, len(s.run))
	for uid, r := range s.run {
		snap[uid] = *r
	}
	s.mu.Unlock()
	now := s.now()
	if now.Sub(s.lastSweep) >= sweepEvery {
		s.lastSweep = now
		s.TrashIdle(ctx)
	}
	parallel := s.cfg.ReapParallel
	if parallel < 1 {
		parallel = 4
	}
	uids := make(chan string, len(snap))
	for uid := range snap {
		uids <- uid
	}
	close(uids)
	var workers sync.WaitGroup
	for range min(parallel, len(snap)) {
		workers.Go(func() {
			for uid := range uids {
				if ctx.Err() != nil {
					return
				}
				s.reapPerson(ctx, uid, snap[uid], now)
			}
		})
	}
	workers.Wait()
	s.sweepTools(ctx)
	s.sweepOffice(ctx)
	s.sweepCAD(ctx)
	s.retryAdoption(ctx)
	if s.AfterReap != nil {
		s.AfterReap(ctx)
	}
}

func (s *Service) reapPerson(ctx context.Context, uid string, r running, now time.Time) {
	ctx = logx.With(ctx, logx.Fields{UID: uid, Slot: r.slot})
	act := decide(now, r, s.cfg)
	s.log.DebugContext(ctx, "reap decision", "action", act.String(), "idle_s", int(now.Sub(r.lastRequest).Seconds()),
		"age_s", int(now.Sub(r.started).Seconds()), "streams", r.streams, "draining", r.draining)
	// the deadlines come first: pending cleanup and purge must not postpone
	// them, and lifetime + DrainCap holds even if draining never began
	if now.Sub(r.started) > s.cfg.MaxLifetime+s.cfg.DrainCap || (act == drainOrKill && now.Sub(r.drainSince) > s.cfg.DrainCap) {
		s.stopIf(ctx, uid, "drain", true, now)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, 6*s.callTimeout)
	s.finishPendingIfFree(pctx, uid)
	cancel()
	s.purgeExpired(ctx, uid)
	switch act {
	case keep:
		s.archiveIfIdle(ctx, uid)
		s.collectIfIdle(ctx, uid, r)
	case beginDrain:
		s.Drain(uid)
	case stopIfIdle:
		s.stopIf(ctx, uid, "idle", false, now)
	case drainOrKill:
		s.stopIf(ctx, uid, "drain", false, now)
	}
}

// stopIf stops when no agent runs (fresh check right before the stop). With
// force, running sessions are interrupted first. A busy per-person lock (a
// start or stop in progress) skips this tick.
func (s *Service) stopIf(ctx context.Context, uid, reason string, force bool, now time.Time) {
	l := s.lock(uid)
	if !l.TryLock() {
		s.log.DebugContext(ctx, "stop skipped", "reason", "start or stop in progress", "stop_reason", reason)
		return
	}
	defer l.Unlock()
	r := s.get(uid)
	if r == nil {
		return
	}
	c := s.client(r)
	cctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	active, err := c.Active(cctx)
	cancel()
	switch {
	case err != nil && !force:
		s.log.WarnContext(ctx, "idle check failed", "slot", r.slot, "err", err)
		return
	case err != nil:
		// past the drain deadline an unanswering API must not keep the slot
		s.log.WarnContext(ctx, "idle check failed, stopping anyway", "slot", r.slot, "err", err)
	case len(active) > 0:
		s.log.DebugContext(ctx, "agents running", "active", len(active), "stop_reason", reason, "force", force)
		if !force {
			return
		}
		ictx, cancel := context.WithTimeout(ctx, s.callTimeout) // all interrupts together
		for _, id := range active {
			if ictx.Err() != nil {
				break
			}
			c.SessionInterrupt(ictx, id)
		}
		cancel()
		time.Sleep(s.interruptWait)
	}
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, 6*s.callTimeout)
		s.collect(cctx, uid) // last look at the chats before the container goes
		cancel()
	}
	if !force {
		cctx, cancel := context.WithTimeout(ctx, s.callTimeout)
		active, err := c.Active(cctx)
		cancel()
		if err != nil {
			s.log.WarnContext(ctx, "idle recheck failed", "slot", r.slot, "err", err)
			return
		}
		s.mu.Lock()
		cur := s.run[uid]
		keep := cur == nil || cur.lastRequest.After(now) || len(active) > 0
		s.mu.Unlock()
		if keep {
			return
		}
	}
	if err := s.stopLocked(ctx, uid, reason); err != nil {
		s.log.ErrorContext(ctx, "stop failed", "op", "stop", "reason", reason, "slot", r.slot, "err", err)
	}
}

// Reconcile adopts running managed containers after a manager restart and
// removes stopped ones and orphans (docs/adr/README.md §3.12). Call before serving.
func (s *Service) Reconcile(ctx context.Context) error {
	list, err := s.rt.List(ctx)
	if err != nil {
		return err
	}
	var officeCleanup sync.WaitGroup
	defer officeCleanup.Wait()
	for _, c := range list {
		if ctx.Err() != nil {
			s.log.WarnContext(ctx, "reconcile deadline reached, keeping remaining containers", "err", ctx.Err())
			return nil
		}
		reconcile := func() {
			if err := s.reconcileContainer(ctx, c); err != nil {
				s.log.WarnContext(ctx, "container reconcile failed", "name", c.Name, "err", err)
			}
		}
		if c.Labels["oc.office"] != "" || c.Labels["oc.freecad"] == "1" {
			officeCleanup.Go(reconcile)
		} else {
			reconcile()
		}
	}
	officeCleanup.Wait()
	slots, err := s.st.UserSlots(ctx)
	if err != nil {
		return err
	}
	for _, slot := range slots {
		changed, err := s.reconcileNetwork(ctx, slot)
		if err != nil {
			s.log.WarnContext(ctx, "network reconcile failed", "slot", slot, "err", err)
			continue
		}
		if changed {
			if err := s.attachNetwork(ctx, slot); err != nil {
				s.log.WarnContext(ctx, "network attach failed", "slot", slot, "err", err)
			}
		}
	}
	return nil
}

func (s *Service) reconcileContainer(ctx context.Context, c runtime.Container) error {
	uid := c.Labels["oc.uid"]
	u, err := s.st.UserByUID(ctx, uid)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil && c.Running && c.Name == config.SlotContainer(u.Slot) && c.Labels["oc.slot"] == strconv.Itoa(u.Slot) {
		// The manager's own network attachments are gone after it was recreated.
		if err := s.attachNetwork(ctx, u.Slot); err != nil {
			return err
		}
		if r, err := s.adoptable(ctx, u, c); err != nil {
			return err
		} else if r != nil {
			s.set(uid, r)
			s.log.InfoContext(ctx, "container adopted", "uid", uid, "slot", u.Slot, "reason", "reconcile", "generation", r.gen)
			return nil
		}
	}
	s.log.WarnContext(ctx, "removing container", "reason", "reconcile: stopped, orphan or not adoptable", "name", c.Name, "running", c.Running, "known_user", err == nil)
	if c.Labels["oc.office"] != "" || c.Labels["oc.freecad"] == "1" {
		return s.removeOffice(ctx, c.ID)
	}
	return s.rt.Remove(ctx, c.ID)
}

// Retry inconclusive startup probes without competing with a person's start or stop.
func (s *Service) retryAdoption(ctx context.Context) {
	list, err := s.rt.List(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "adoption retry: list", "err", err)
		return
	}
	for _, c := range list {
		if ctx.Err() != nil {
			return
		}
		if !c.Running || isToolbox(c.Labels) || c.Name != "oc-user"+c.Labels["oc.slot"] {
			continue
		}
		uid := c.Labels["oc.uid"]
		l := s.lock(uid)
		if !l.TryLock() {
			continue
		}
		if s.get(uid) == nil {
			// A stop may have completed between List and TryLock.
			current, err := s.rt.Inspect(ctx, c.Name)
			if err == nil && current.ID == c.ID && current.Running {
				err = s.reconcileContainer(ctx, current)
			}
			if err != nil && !errors.Is(err, runtime.ErrNotFound) {
				s.log.WarnContext(ctx, "container adoption retry failed", "name", c.Name, "err", err)
			}
		}
		l.Unlock()
	}
}

// Network options are immutable; never detach a person's running containers.
func (s *Service) reconcileNetwork(ctx context.Context, slot int) (bool, error) {
	name := config.SlotNetwork(slot)
	mode, err := s.rt.NetworkGatewayMode(ctx, name)
	if errors.Is(err, runtime.ErrNotFound) {
		return false, nil
	}
	if err != nil || mode == s.cfg.NetGatewayMode || s.cfg.NetGatewayMode == "" {
		return false, err
	}
	containers, err := s.rt.List(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range containers {
		if c.Running && (c.Labels["oc.slot"] == strconv.Itoa(slot) || c.Name == config.SlotContainer(slot)) {
			if _, logged := s.networkDeferred.LoadOrStore(name, true); !logged {
				s.log.WarnContext(ctx, "network isolation deferred until containers stop", "network", name)
			}
			return false, nil
		}
	}
	if err := s.rt.RemoveNetwork(ctx, name); err != nil {
		return false, err
	}
	if err := s.rt.EnsureNetwork(ctx, name, s.cfg.SlotSubnet(slot), s.cfg.NetGatewayMode); err != nil {
		return false, err
	}
	s.networkDeferred.Delete(name)
	return true, nil
}

// Loop runs the watchdog and Reap every ReapInterval until ctx ends.
func (s *Service) Loop(ctx context.Context) {
	var watchdog sync.WaitGroup
	watchdog.Go(func() { s.WatchdogLoop(ctx) })
	defer watchdog.Wait()
	t := time.NewTicker(s.cfg.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Reap(ctx)
		}
	}
}
