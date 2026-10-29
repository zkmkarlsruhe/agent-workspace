// SPDX-License-Identifier: MIT

package lifecycle

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
)

// WatchdogNotice contains no model output or retry error message.
type WatchdogNotice struct {
	SessionID     string
	ParentID      string
	Title         string
	Directory     string
	SilentSeconds int
	Attempt       int
	Retry         bool
	Next          time.Time
}

type watchedSession struct {
	info          ocapi.Session
	last          time.Time
	retry         time.Time
	attempt       int
	reported      bool
	waiting       bool
	retryReported time.Time
}

type watchdog struct {
	mu        sync.Mutex
	silent    time.Duration
	connected bool
	revision  uint64
	sessions  map[string]*watchedSession
}

func newWatchdog(silent time.Duration) *watchdog {
	if silent <= 0 {
		silent = 180 * time.Second
	}
	return &watchdog{silent: silent, sessions: map[string]*watchedSession{}}
}

func (d *watchdog) connection(connected bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// shortcut: history is memory-only; add replay if reconnects must preserve the window.
	d.connected = connected
	d.sessions = map[string]*watchedSession{}
	d.revision++
}

func (d *watchdog) version() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.revision
}

// A snapshot cannot resurrect or remove sessions across a lifecycle transition.
func (d *watchdog) reconcile(version uint64, sessions []ocapi.Session, waiting map[string]bool, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.connected {
		return
	}
	active := map[string]bool{}
	for _, info := range sessions {
		active[info.ID] = true
		if se := d.sessions[info.ID]; se != nil {
			se.info = info
			if d.revision == version {
				if se.waiting != waiting[info.ID] {
					se.last, se.reported = now, false
				}
				se.waiting = waiting[info.ID]
			}
		} else if d.revision == version && len(d.sessions) < 4096 {
			d.sessions[info.ID] = &watchedSession{info: info, last: now, waiting: waiting[info.ID]}
		}
	}
	for id := range d.sessions {
		if d.revision == version && !active[id] {
			delete(d.sessions, id)
		}
	}
	d.revision++
}

func progressEvent(kind string) bool {
	switch kind {
	case "session.step.started", "session.step.streamed", "session.step.ended", "session.step.failed",
		"session.text.started", "session.text.delta", "session.text.ended",
		"session.reasoning.started", "session.reasoning.delta", "session.reasoning.ended",
		"session.tool.input.started", "session.tool.input.delta", "session.tool.input.ended",
		"session.tool.called", "session.tool.progress", "session.tool.success", "session.tool.failed":
		return true
	}
	return false
}

func (d *watchdog) event(e ocapi.Event, now time.Time) {
	progress := progressEvent(e.Type)
	if !progress && e.Type != "session.status" && e.Type != "session.retry.scheduled" &&
		!strings.HasPrefix(e.Type, "session.execution.") && e.Type != "session.deleted" && e.Type != "session.idle" {
		return
	}
	var data struct {
		SessionID string `json:"sessionID"`
		Agent     string `json:"agent"`
		Attempt   int    `json:"attempt"`
		At        int64  `json:"at"`
		Status    struct {
			Type    string `json:"type"`
			Attempt int    `json:"attempt"`
			Next    int64  `json:"next"`
		} `json:"status"`
	}
	if json.Unmarshal(e.Data, &data) != nil || data.SessionID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.connected {
		return
	}
	if !progress {
		d.revision++
	}
	id := data.SessionID
	switch e.Type {
	case "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted", "session.deleted", "session.idle":
		delete(d.sessions, id)
		return
	case "session.status":
		if data.Status.Type == "idle" {
			delete(d.sessions, id)
			return
		}
		if data.Status.Type != "busy" && data.Status.Type != "retry" {
			return
		}
	}
	se := d.sessions[id]
	if se == nil {
		if e.Type != "session.execution.started" && e.Type != "session.status" && e.Type != "session.retry.scheduled" {
			return
		}
		if len(d.sessions) >= 4096 {
			return // bound memory even if a container sends invented IDs
		}
		se = &watchedSession{info: ocapi.Session{ID: id}, last: now}
		d.sessions[id] = se
	}
	if data.Agent != "" {
		se.info.Agent = data.Agent
	}
	if progress || e.Type == "session.execution.started" || (data.Status.Type == "busy" && !se.retry.IsZero()) {
		se.last, se.retry, se.attempt, se.reported = now, time.Time{}, 0, false
	}
	if e.Type == "session.retry.scheduled" || data.Status.Type == "retry" {
		attempt, next := data.Attempt, data.At
		if e.Type == "session.status" {
			attempt, next = data.Status.Attempt, data.Status.Next
		}
		if attempt >= 0 && next > 0 {
			se.retry, se.attempt, se.reported = time.UnixMilli(next), attempt, false
			se.last = now
		}
	}
}

// snapshot returns notices and the detections not yet logged. Metadata must
// come from session.get before a session can be shown or stopped.
func (d *watchdog) snapshot(now time.Time) (notices, detected []WatchdogNotice) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.connected {
		return
	}
	for id, se := range d.sessions {
		if se.info.Location.Directory == "" || se.waiting {
			continue
		}
		title := se.info.Title
		if title == "" {
			title = se.info.Agent
		}
		if title == "" {
			title = id
		}
		n := WatchdogNotice{SessionID: id, ParentID: se.info.ParentID, Title: title, Directory: se.info.Location.Directory}
		if se.retry.After(now) {
			n.Retry, n.Attempt, n.Next = true, se.attempt, se.retry
			if !se.retryReported.Equal(se.retry) {
				se.retryReported = se.retry
				detected = append(detected, n)
			}
		} else {
			last := se.last
			if se.retry.After(last) {
				last = se.retry
			}
			if now.Sub(last) < d.silent {
				continue
			}
			n.SilentSeconds = max(1, int(now.Sub(last).Seconds()))
			if !se.reported {
				se.reported = true
				detected = append(detected, n)
			}
		}
		notices = append(notices, n)
	}
	slices.SortFunc(notices, func(a, b WatchdogNotice) int { return strings.Compare(a.SessionID, b.SessionID) })
	return
}

func (d *watchdog) needsMetadata() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, se := range d.sessions {
		if se.info.Location.Directory == "" {
			return true
		}
	}
	return false
}
