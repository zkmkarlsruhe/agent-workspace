// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// archiveIfIdle runs daily per running generation (sooner for sub-day thresholds).
// Busy environments and failures retry on the next reaper tick.
// No container is started or kept alive for this work.
func (s *Service) archiveIfIdle(ctx context.Context, uid string) {
	days := instance.Current().Sessions.ArchiveAfterDays
	if days == 0 {
		return
	}
	l := s.lock(uid)
	if !l.TryLock() {
		return
	}
	defer l.Unlock()
	after := time.Duration(days * float64(24*time.Hour))
	r := s.get(uid)
	if r == nil || r.draining || s.now().Sub(r.lastArchiveScan) < min(sweepEvery, after) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 6*s.callTimeout)
	defer cancel()
	archived, err := s.archiveDormant(ctx, uid, s.client(r), s.now().Add(-after))
	if err != nil {
		if !errors.Is(err, ErrBusy) {
			s.log.WarnContext(ctx, "session archive sweep failed", "uid", uid, "err", err)
		}
		if archived == 0 {
			return
		}
	}
	// Partial sweeps made progress; avoid retrying a large backlog every tick.
	s.mu.Lock()
	if cur := s.run[uid]; cur != nil {
		cur.lastArchiveScan = s.now()
	}
	s.mu.Unlock()
}

func archiveCandidate(se ocapi.Session, dir string, cutoff time.Time) bool {
	last := sessionTime(se)
	return se.ParentID == "" && se.Location.Directory == dir && se.Time.Archived == nil &&
		!last.IsZero() && last.Before(cutoff)
}

// An active child or a run in another project defers the whole sweep.
func archiveIdle(ctx context.Context, c *ocapi.Client) error {
	active, err := c.Active(ctx)
	if err != nil {
		return err
	}
	if len(active) > 0 {
		return ErrBusy
	}
	return nil
}

func (s *Service) archiveDormant(ctx context.Context, uid string, c *ocapi.Client, cutoff time.Time) (archived int, err error) {
	if err := archiveIdle(ctx, c); err != nil {
		return archived, err
	}
	ps, err := s.st.Projects(ctx, uid)
	if err != nil {
		return archived, err
	}
	for _, p := range ps {
		if p.Isolated || p.Assistant.ID != "" {
			continue
		}
		dir := s.projectDir(p.Folder)
		sessions, err := c.SessionListAll(ctx, dir)
		if err != nil {
			return archived, err
		}
		for _, se := range sessions {
			if !archiveCandidate(se, dir, cutoff) {
				continue
			}
			// Re-read after listing: the person may have continued or moved the chat.
			fresh, err := c.SessionGet(ctx, se.ID)
			if ocapi.IsStatus(err, 404) {
				continue
			}
			if err != nil {
				return archived, err
			}
			if fresh.ID != se.ID || !archiveCandidate(fresh, dir, cutoff) {
				continue
			}
			if err := archiveIdle(ctx, c); err != nil {
				return archived, err
			}
			// shortcut: OpenCode has no conditional archive; a run starting after
			// this check can race the PATCH. Use an atomic idle guard when available.
			if err := c.SessionArchive(ctx, se.ID, true); err != nil {
				return archived, err
			}
			archived++
			s.log.InfoContext(ctx, "session archived", "op", "session_archive", "uid", uid,
				"project_id", p.ID, "session_id", se.ID, "last_activity", sessionTime(fresh))
		}
	}
	return archived, nil
}

// RestoreSession only accepts a root chat in this owner's project and running
// environment. Unarchive updates OpenCode's activity time, giving it a full grace period.
func (s *Service) RestoreSession(ctx context.Context, uid, project, session string) error {
	if !ValidProject(project) || session == "" {
		return ErrNotFound
	}
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	p, err := s.st.ProjectByFolder(ctx, uid, project)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if p.Isolated {
		return ErrNotFound
	}
	c, ok := s.Client(uid)
	if !ok {
		return ErrNotRunning
	}
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	defer cancel()
	se, err := c.SessionGet(ctx, session)
	if ocapi.IsStatus(err, 404) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if se.ID != session || se.ParentID != "" || se.Location.Directory != s.projectDir(project) {
		return ErrNotFound
	}
	if se.Time.Archived == nil {
		return nil
	}
	if err := c.SessionArchive(ctx, session, false); err != nil {
		return err
	}
	s.Touch(uid)
	s.touchProject(ctx, uid, project, s.now())
	s.log.InfoContext(ctx, "session unarchived", "op", "session_unarchive", "uid", uid,
		"project_id", p.ID, "session_id", session)
	return nil
}
