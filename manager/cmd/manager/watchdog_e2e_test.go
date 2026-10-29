// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
)

func TestWatchdogE2E(t *testing.T) {
	s := newStack(t, "127.94", func(c *config.Config) { c.WatchdogSilent = time.Second })
	want(t, s.do(t, call{url: "/", as: &alice}), http.StatusOK)
	s.waitRunning(t)
	wantLoc(t, startProject(t, s, "Summarise the documents in a report."), "https://alice.apps.example.org/server/")
	const project = "besucher-auswertung"
	s.oc.Mu.Lock()
	parent := s.oc.Sessions[0].ID
	s.oc.Sessions = append(s.oc.Sessions, ocapi.Session{ID: "ses_child", ParentID: parent, Title: "Research",
		Location: ocapi.Location{Directory: "/workspace/" + project}})
	s.oc.Mu.Unlock()
	s.workers.Go(func() { s.svc.WatchdogLoop(s.ctx) })
	waitWatchdog(t, func() bool { return s.oc.EventReaders() == 1 })
	s.oc.Emit("session.execution.started", map[string]any{"sessionID": parent})
	s.oc.Emit("session.execution.started", map[string]any{"sessionID": "ses_child"})
	s.oc.Emit("session.step.started", map[string]any{"sessionID": "ses_child", "agent": "research"})
	fragment := "/projects/" + project + "/watchdog"
	waitWatchdog(t, func() bool {
		w := s.do(t, call{url: fragment, as: &alice, hdr: []string{"Accept-Language", "en"}})
		return w.Code == http.StatusOK && strings.Contains(w.Body.String(), "Subagent Research has produced no output")
	})
	want(t, s.do(t, call{url: "/projects/" + project, as: &alice, hdr: []string{"Accept-Language", "en"}}),
		http.StatusOK, "Subagent Research has produced no output", "Stop subagent")
	// A broken stream is not evidence of silence. Reconnect reconciles the
	// still-active child, even though its started event is not replayed.
	s.oc.DisconnectEvents()
	waitWatchdog(t, func() bool {
		w := s.do(t, call{url: fragment, as: &alice})
		return w.Code == http.StatusOK && !strings.Contains(w.Body.String(), "Research")
	})
	waitWatchdog(t, func() bool {
		w := s.do(t, call{url: fragment, as: &alice, hdr: []string{"Accept-Language", "en"}})
		return w.Code == http.StatusOK && strings.Contains(w.Body.String(), "Subagent Research has produced no output")
	})
	// A preview host cannot turn the notice into a stop request.
	stop := fragment + "/ses_child/stop"
	want(t, s.do(t, call{method: "POST", url: stop, as: &alice, hdr: []string{"Origin", "https://alice--8000.apps.example.org"}}), http.StatusForbidden)
	w := s.do(t, call{method: "POST", url: stop, as: &alice, hdr: []string{"Origin", "https://apps.example.org", "HX-Request", "true"}})
	want(t, w, http.StatusOK)
	s.oc.Mu.Lock()
	interrupts := slices.Clone(s.oc.Interrupted)
	active := slices.Clone(s.oc.Active)
	s.oc.Mu.Unlock()
	if !slices.Equal(interrupts, []string{"/api/session/ses_child/interrupt"}) || !slices.Contains(active, parent) {
		t.Fatalf("interrupts %v active %v", interrupts, active)
	}
	waitWatchdog(t, func() bool {
		w := s.do(t, call{url: fragment, as: &alice})
		return w.Code == http.StatusOK && !strings.Contains(w.Body.String(), "Research")
	})
	// Model-side continuation after the cancelled foreground tool: the parent
	// produces another step and finishes; no interrupt was sent to it.
	s.oc.Emit("session.tool.failed", map[string]any{"sessionID": parent})
	s.oc.Emit("session.step.started", map[string]any{"sessionID": parent})
	s.oc.Emit("session.text.delta", map[string]any{"sessionID": parent, "delta": "Report complete"})
	s.oc.Emit("session.execution.succeeded", map[string]any{"sessionID": parent})
	waitWatchdog(t, func() bool {
		w := s.do(t, call{url: fragment, as: &alice})
		return w.Code == http.StatusOK && !strings.Contains(w.Body.String(), `class="alert"`)
	})
	s.oc.Mu.Lock()
	remaining, stopped := len(s.oc.Active), len(s.oc.Interrupted)
	s.oc.Mu.Unlock()
	if remaining != 0 || stopped != 1 {
		t.Fatalf("active %d interrupts %d", remaining, stopped)
	}
}

func waitWatchdog(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("watchdog state did not converge")
}
