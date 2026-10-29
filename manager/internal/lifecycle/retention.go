// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// sweepEvery is how often Reap runs the inactivity rule; the first tick after
// a manager start runs it too.
const sweepEvery = 24 * time.Hour

// touchProject records activity on a project for the inactivity rule.
func (s *Service) touchProject(ctx context.Context, uid, project string, t time.Time) {
	if t.IsZero() {
		return
	}
	if err := s.st.TouchProject(context.WithoutCancel(ctx), uid, project, t); err != nil {
		s.log.WarnContext(ctx, "touch project failed", "err", err)
	}
}

// TrashIdle moves every project without activity for config.ProjectIdleTrash
// to the trash (docs/adr/README.md: inactivity cleanup). It works on the store
// only: no container is started or touched. The folder moves into the trash
// dir the next time the person's environment runs (finishPending); until
// then the item is pending. Idempotent: a moved project is no longer a project.
func (s *Service) TrashIdle(ctx context.Context) {
	now := s.now()
	if err := s.st.PurgeAssistantGaps(ctx, now); err != nil {
		s.log.WarnContext(ctx, "assistant gap retention failed", "err", err)
	}
	expired, err := s.st.ExpiredAssistantTrash(ctx, now)
	if err != nil {
		s.log.WarnContext(ctx, "support retention failed", "err", err)
		return
	}
	for _, t := range expired {
		if err := s.st.DeleteTrash(ctx, t.Project.UID, t.ID); err != nil {
			s.log.WarnContext(ctx, "support purge failed", "trash", t.ID, "err", err)
		}
	}
	ps, err := s.st.IdleProjects(ctx, now.Add(-config.ProjectIdleTrash))
	if err != nil {
		s.log.WarnContext(ctx, "idle projects failed", "err", err)
		return
	}
	for _, p := range ps {
		id := newID()
		if err := s.st.MoveToTrash(ctx, p, id, now, now.Add(config.TrashRetention), nil, true, !p.Isolated); err != nil {
			s.log.WarnContext(ctx, "idle trash failed", "project_id", p.ID, "err", err)
			continue
		}
		s.log.InfoContext(ctx, "project trashed for inactivity", "project_id", p.ID, "trash", id, "last_activity", p.LastActivity)
	}
}

// finishPending moves the folders of pending trash items into the trash dir.
func (s *Service) finishPending(ctx context.Context, uid string) {
	l := s.lock(uid)
	l.Lock()
	defer l.Unlock()
	s.finishPendingLocked(ctx, uid)
}

// finishPendingIfFree is finishPending for Reap: a busy lock skips the tick.
func (s *Service) finishPendingIfFree(ctx context.Context, uid string) {
	l := s.lock(uid)
	if !l.TryLock() {
		return
	}
	defer l.Unlock()
	s.finishPendingLocked(ctx, uid)
}

// finishPendingLocked runs with the person's lock held on a running
// environment. It records the folder's sessions with the item, as a manual
// move does. An item whose folder has a running agent waits; a missing
// folder only clears the mark. Failures are logged and retried next time.
func (s *Service) finishPendingLocked(ctx context.Context, uid string) {
	ts, err := s.st.Trash(ctx, uid)
	if err != nil {
		s.log.WarnContext(ctx, "trash list failed", "err", err)
		return
	}
	ts = slices.DeleteFunc(ts, func(t store.TrashItem) bool { return !t.Pending })
	if len(ts) == 0 {
		return
	}
	c, ok := s.Client(uid)
	if !ok {
		return
	}
	active, err := c.Active(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "pending trash: active failed", "err", err)
		return
	}
	for _, t := range ts {
		sess, err := c.SessionListAll(ctx, s.projectDir(t.Project.Folder))
		if err != nil {
			s.log.WarnContext(ctx, "pending trash: sessions failed", "trash", t.ID, "err", err)
			continue
		}
		keep := make([]store.TrashSession, 0, len(sess))
		busy := false
		for _, se := range sess {
			busy = busy || slices.Contains(active, se.ID)
			keep = append(keep, store.TrashSession{ID: se.ID, Root: se.ParentID == ""})
		}
		if busy {
			continue
		}
		err = s.sh(ctx, uid, `[ -d "$1" ] && [ ! -L "$1" ] || exit 3; mkdir -p -- "$(dirname -- "$2")" && mv -T -- "$1" "$2"`,
			nil, nil, s.projectDir(t.Project.Folder), s.trashDir(t.ID))
		if err != nil && !errors.Is(err, ErrNotFound) {
			s.log.WarnContext(ctx, "pending trash: move failed", "trash", t.ID, "err", err)
			continue
		}
		s.evictRemovedLocation(ctx, uid, s.projectDir(t.Project.Folder))
		if err := s.st.FinishPending(ctx, t.ID, keep); err != nil {
			s.log.WarnContext(ctx, "pending trash: store failed", "trash", t.ID, "err", err)
		}
	}
}
