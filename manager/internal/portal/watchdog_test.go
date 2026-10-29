// SPDX-License-Identifier: MIT

package portal

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWatchdogPagesAndLanguages(t *testing.T) {
	h, f := setup(t)
	f.addProject("test", "")
	f.watchdog = []WatchdogNotice{
		{SessionID: "child", ParentID: "root", Title: `<script>alert(1)</script>`, SilentSeconds: 240},
		{SessionID: "root", Title: "Main", SilentSeconds: 240},
		{SessionID: "retry", Title: "Retry", Attempt: 3, Retry: true, Next: time.Now().Add(20 * time.Second)},
	}
	for _, path := range []string{"/", "/projects/test", "/environment", "/projects/test/watchdog", "/watchdog"} {
		w := do(t, h, req{path: path, header: map[string]string{"Accept-Language": "en"}})
		wantStatus(t, w, http.StatusOK)
		wantBody(t, w, "has produced no output for 4 min", "Stop subagent", "Model is being retried (attempt 3, next try in", "Background subagents must be stopped one by one.")
		if strings.Contains(w.Body.String(), "hx-trigger") {
			t.Fatalf("%s: watchdog must not poll", path)
		}
		if strings.Contains(w.Body.String(), "<script>alert") {
			t.Fatal("session title not escaped")
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Fatal("CSP missing")
		}
	}
	w := do(t, h, req{path: "/projects/test/watchdog"})
	wantBody(t, w, "hat seit 4 Min. keine Ausgabe erzeugt", "Subagent stoppen", "Versuch 3")
	if f.watchdogUID != anna.UID || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("owner %q cache %q", f.watchdogUID, w.Header().Get("Cache-Control"))
	}
}

func TestWatchdogStopProtection(t *testing.T) {
	h, f := setup(t)
	f.addProject("test", "")
	f.watchdog = []WatchdogNotice{{SessionID: "child", ParentID: "root", SilentSeconds: 240}}
	path := "/projects/test/watchdog/child/stop"
	for _, headers := range []map[string]string{
		{"Origin": "https://other.example"},
		{"Sec-Fetch-Site": "same-site", "Origin": "https://preview.apps.example.org"},
	} {
		wantStatus(t, do(t, h, req{method: "POST", path: path, header: headers}), http.StatusForbidden)
	}
	wantStatus(t, do(t, h, req{method: "POST", path: path, id: &Identity{}}), http.StatusForbidden)
	wantStatus(t, do(t, h, req{path: path}), http.StatusNotFound)
	if len(f.watchdogStops) != 0 {
		t.Fatal("rejected request reached backend", f.watchdogStops)
	}
	w := do(t, h, req{method: "POST", path: path, header: map[string]string{"Origin": "https://apps.example.org", "HX-Request": "true"}})
	wantStatus(t, w, http.StatusOK)
	if len(f.watchdogStops) != 1 || f.watchdogStops[0] != "child" || f.watchdogUID != anna.UID || strings.Contains(w.Body.String(), "Subagent stoppen") {
		t.Fatalf("stops %v owner %s body %s", f.watchdogStops, f.watchdogUID, w.Body.String())
	}
	wantStatus(t, do(t, h, req{method: "POST", path: path}), http.StatusNotFound)
	f.watchdog = []WatchdogNotice{{SessionID: "root", SilentSeconds: 240}}
	w = do(t, h, req{method: "POST", path: "/watchdog/root/stop"})
	wantStatus(t, w, http.StatusSeeOther)
	if w.Header().Get("Location") != "/environment" {
		t.Fatal(w.Header())
	}
}

func TestWatchdogEmptyAndRetryOnly(t *testing.T) {
	h, f := setup(t)
	w := do(t, h, req{path: "/watchdog"})
	wantStatus(t, w, http.StatusOK)
	if strings.Contains(w.Body.String(), "<form") || strings.Contains(w.Body.String(), `class="alert"`) {
		t.Fatal(w.Body.String())
	}
	f.watchdog = []WatchdogNotice{{SessionID: "retry", Attempt: 3, Retry: true, Next: time.Now().Add(time.Minute)}}
	w = do(t, h, req{path: "/watchdog"})
	if strings.Contains(w.Body.String(), "<form") {
		t.Fatal("retry alone must not offer a silence stop")
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/watchdog/retry/stop"}), http.StatusNotFound)
	wantStatus(t, do(t, h, req{path: "/projects/missing/watchdog"}), http.StatusNotFound)
}

func TestWatchdogRefreshOnDemand(t *testing.T) {
	h, f := setup(t)
	f.addProject("test", "")
	refresh := `href="/projects/test" hx-get="/projects/test/watchdog" hx-target="#watchdog" hx-swap="outerHTML"`
	f.projects["test"].Status = StatusWorking
	w := do(t, h, req{path: "/projects/test"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, refresh)
	if strings.Contains(w.Body.String(), "hx-trigger") {
		t.Fatal("active project must refresh only on demand")
	}
	f.projects["test"].Status = StatusNone
	w = do(t, h, req{path: "/projects/test"})
	if strings.Contains(w.Body.String(), refresh) {
		t.Fatal("idle project without notices gained a refresh control")
	}
	// A silent background child is actionable even after its root went idle.
	f.watchdog = []WatchdogNotice{{SessionID: "child", ParentID: "root", SilentSeconds: 240}}
	wantBody(t, do(t, h, req{path: "/projects/test"}), refresh)
	for _, state := range []EnvState{EnvRunning, EnvStopped, EnvStarting, EnvError} {
		f.env = EnvStatus{State: state}
		for _, path := range []string{"/", "/environment"} {
			b := do(t, h, req{path: path}).Body.String()
			if strings.Contains(b, `hx-get="/watchdog"`) || strings.Contains(b, `hx-get="/projects/test/watchdog"`) {
				t.Fatalf("%s %s: watchdog refresh leaked onto overview", state, path)
			}
			if (path != "/" || state != EnvStarting) && strings.Contains(b, "hx-trigger") {
				t.Fatalf("%s %s: only starting cards may poll", state, path)
			}
		}
	}
}
