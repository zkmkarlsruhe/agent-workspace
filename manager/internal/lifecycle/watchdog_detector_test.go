// SPDX-License-Identifier: MIT

package lifecycle

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
)

func watchdogEvent(kind, id string, fields map[string]any) ocapi.Event {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["sessionID"] = id
	data, _ := json.Marshal(fields)
	return ocapi.Event{Type: kind, Data: data}
}

func watchSession(id, parent string) ocapi.Session {
	return ocapi.Session{ID: id, ParentID: parent, Title: id, Location: ocapi.Location{Directory: "/workspace/test"}}
}

func TestWatchdogSilenceAndReasoning(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newWatchdog(180 * time.Second)
	d.connection(true)
	d.reconcile(d.version(), []ocapi.Session{watchSession("root", ""), watchSession("child", "root")}, nil, now)
	if n, _ := d.snapshot(now.Add(179 * time.Second)); len(n) != 0 {
		t.Fatal(n)
	}
	now = now.Add(4 * time.Minute)
	d.event(watchdogEvent("session.status", "root", map[string]any{"status": map[string]any{"type": "busy"}}), now)
	n, detected := d.snapshot(now)
	if len(n) != 2 || len(detected) != 2 || n[0].ParentID != "root" || n[0].SilentSeconds != 240 {
		t.Fatalf("notices %+v detections %+v", n, detected)
	}
	if _, detected := d.snapshot(now); len(detected) != 0 {
		t.Fatal("duplicate detection", detected)
	}
	// Reasoning can be quiet for minutes and then resume without being stuck.
	d.event(watchdogEvent("session.reasoning.delta", "child", map[string]any{"delta": "never retained"}), now)
	n, _ = d.snapshot(now)
	if len(n) != 1 || n[0].SessionID != "root" {
		t.Fatal(n)
	}
	d.event(watchdogEvent("session.execution.interrupted", "root", nil), now)
	n, detected = d.snapshot(now.Add(180 * time.Second))
	if len(n) != 1 || n[0].SessionID != "child" || len(detected) != 1 {
		t.Fatal("parent termination must not clear a background child", n, detected)
	}
	d.event(watchdogEvent("session.text.delta", "child", nil), now.Add(181*time.Second))
	if n, _ := d.snapshot(now.Add(182 * time.Second)); len(n) != 0 {
		t.Fatal(n)
	}
}

func TestWatchdogRetry(t *testing.T) {
	for _, kind := range []string{"session.retry.scheduled", "session.status"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Unix(1000, 0)
			d := newWatchdog(180 * time.Second)
			d.connection(true)
			d.reconcile(d.version(), []ocapi.Session{watchSession("root", "")}, nil, now)
			next := now.Add(10 * time.Minute)
			fields := map[string]any{"attempt": 3, "at": next.UnixMilli()}
			if kind == "session.status" {
				fields = map[string]any{"status": map[string]any{"type": "retry", "attempt": 3, "next": next.UnixMilli(), "message": "private error"}}
			}
			d.event(watchdogEvent(kind, "root", fields), now)
			n, detected := d.snapshot(now.Add(5 * time.Minute))
			if len(n) != 1 || n[0].Attempt != 3 || !n[0].Next.Equal(next) || n[0].SilentSeconds != 0 || len(detected) != 1 {
				t.Fatal(n, detected)
			}
			if _, detected := d.snapshot(now.Add(6 * time.Minute)); len(detected) != 0 {
				t.Fatal("duplicate retry detection", detected)
			}
			if n, _ := d.snapshot(next.Add(179 * time.Second)); len(n) != 0 {
				t.Fatal("retry wait counted as silence", n)
			}
			n, detected = d.snapshot(next.Add(180 * time.Second))
			if len(n) != 1 || n[0].SilentSeconds != 180 || len(detected) != 1 {
				t.Fatal(n, detected)
			}
			d.event(watchdogEvent("session.step.started", "root", nil), next.Add(181*time.Second))
			if n, _ := d.snapshot(next.Add(182 * time.Second)); len(n) != 0 {
				t.Fatal(n)
			}
		})
	}
}

func TestWatchdogProgressAndTerminalEvents(t *testing.T) {
	for _, kind := range []string{"session.step.started", "session.step.streamed", "session.step.ended", "session.step.failed",
		"session.text.delta", "session.reasoning.delta", "session.tool.input.delta", "session.tool.called",
		"session.tool.progress", "session.tool.success", "session.tool.failed"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Unix(1000, 0)
			d := newWatchdog(time.Second)
			d.connection(true)
			d.reconcile(d.version(), []ocapi.Session{watchSession("s", "")}, nil, now)
			d.event(watchdogEvent(kind, "s", nil), now.Add(time.Second))
			if n, _ := d.snapshot(now.Add(time.Second)); len(n) != 0 {
				t.Fatal(n)
			}
		})
	}
	for _, kind := range []string{"session.execution.succeeded", "session.execution.failed", "session.execution.interrupted", "session.deleted", "session.idle", "session.status"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Unix(1000, 0)
			d := newWatchdog(time.Second)
			d.connection(true)
			d.reconcile(d.version(), []ocapi.Session{watchSession("s", "")}, nil, now)
			d.event(watchdogEvent(kind, "s", map[string]any{"status": map[string]string{"type": "idle"}}), now)
			if n, _ := d.snapshot(now.Add(time.Hour)); len(n) != 0 {
				t.Fatal(n)
			}
		})
	}
}

func TestWatchdogReconnectSnapshotAndQuestions(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newWatchdog(time.Second)
	d.connection(true)
	info := []ocapi.Session{watchSession("s", "")}
	d.event(watchdogEvent("session.execution.started", "s", nil), now)
	version := d.version()
	d.event(watchdogEvent("session.execution.interrupted", "s", nil), now)
	d.reconcile(version, info, nil, now)
	if n, _ := d.snapshot(now.Add(time.Hour)); len(n) != 0 {
		t.Fatal("stale active snapshot resurrected session", n)
	}
	d.reconcile(d.version(), info, map[string]bool{"s": true}, now)
	if n, _ := d.snapshot(now.Add(time.Hour)); len(n) != 0 {
		t.Fatal("waiting for a person", n)
	}
	d.reconcile(d.version(), info, nil, now.Add(time.Hour))
	if n, _ := d.snapshot(now.Add(time.Hour)); len(n) != 0 {
		t.Fatal("wait counted as silence", n)
	}
	d.connection(false)
	if n, _ := d.snapshot(now.Add(2 * time.Hour)); len(n) != 0 {
		t.Fatal("disconnected stream is not silence", n)
	}
	d.connection(true)
	d.reconcile(d.version(), info, nil, now.Add(2*time.Hour))
	if n, _ := d.snapshot(now.Add(2 * time.Hour)); len(n) != 0 {
		t.Fatal("reconnect must start fresh", n)
	}
	d.event(ocapi.Event{Type: "session.text.delta", Data: json.RawMessage(`{"sessionID":42}`)}, now)
	d.event(ocapi.Event{Type: "server.heartbeat"}, now.Add(3*time.Hour))
	if n, _ := d.snapshot(now.Add(3 * time.Hour)); len(n) != 1 {
		t.Fatal("heartbeat/malformed event reset silence", n)
	}
}

func TestWatchdogLegacyRetryZeroAndBusy(t *testing.T) {
	now := time.Unix(1000, 0)
	d := newWatchdog(time.Second)
	d.connection(true)
	d.reconcile(d.version(), []ocapi.Session{watchSession("s", "")}, nil, now)
	d.event(watchdogEvent("session.status", "s", map[string]any{"status": map[string]any{
		"type": "retry", "attempt": 0, "next": now.Add(time.Minute).UnixMilli(),
	}}), now)
	if n, _ := d.snapshot(now); len(n) != 1 || !n[0].Retry || n[0].Attempt != 0 {
		t.Fatal(n)
	}
	d.event(watchdogEvent("session.status", "s", map[string]any{"status": map[string]string{"type": "busy"}}), now)
	if n, _ := d.snapshot(now); len(n) != 0 {
		t.Fatal("busy must end retry", n)
	}
	if n, _ := d.snapshot(now.Add(time.Second)); len(n) != 1 || n[0].Retry {
		t.Fatal(n)
	}
}
