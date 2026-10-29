// SPDX-License-Identifier: MIT

package portal

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeConns struct {
	v           Connection
	err         error
	connects    []string // username|id
	disconnects int
}

func (f *fakeConns) List(context.Context, string) ([]Connection, error) {
	return []Connection{f.v}, nil
}

func (f *fakeConns) Connect(_ context.Context, _, username, id string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.connects = append(f.connects, username+"|"+id)
	f.v.Flow = Flow{State: FlowPending, LoginURL: "https://cloud.example.org/login/v2/flow/abc"}
	return f.v.Flow.LoginURL, nil
}

func (f *fakeConns) Disconnect(context.Context, string, string) error {
	if f.err != nil {
		return f.err
	}
	f.disconnects++
	f.v.Connected = false
	return nil
}

type fakeOnboarding struct{ seen map[string]bool }

func (f *fakeOnboarding) Seen(_ context.Context, uid string) (bool, error) { return f.seen[uid], nil }
func (f *fakeOnboarding) MarkSeen(_ context.Context, uid string) error {
	f.seen[uid] = true
	return nil
}

func setupConns(t *testing.T) (http.Handler, *fakeConns, *fakeOnboarding) {
	t.Helper()
	fc := &fakeConns{v: Connection{ID: "nextcloud", Title: "Nextcloud"}}
	fo := &fakeOnboarding{seen: map[string]bool{}}
	h, err := New(Config{Backend: newFake(), Connections: fc, Onboarding: fo, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	return h, fc, fo
}

func TestConnectionsHiddenWithout(t *testing.T) {
	h, _ := setup(t)
	wantStatus(t, do(t, h, req{path: "/connections"}), http.StatusNotFound)
	wantStatus(t, do(t, h, req{path: "/onboarding"}), http.StatusNotFound)
	w := do(t, h, req{path: "/environment"})
	if strings.Contains(w.Body.String(), `href="/connections"`) || strings.Contains(w.Body.String(), "Einrichtung erneut starten") {
		t.Error("links without the features")
	}
	// no Onboarding: the start page shows directly
	wantStatus(t, do(t, h, req{path: "/"}), http.StatusOK)
}

func TestConnectionsStates(t *testing.T) {
	h, fc, _ := setupConns(t)
	w := do(t, h, req{path: "/connections"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, "<h1>Verbindungen</h1>", "nicht verbunden", `action="/connections/nextcloud/connect"`, ">Verbinden<",
		"Dein Nextcloud-Passwort sieht Agent Workspace nie", `href="/connections" aria-current="page"`)
	if strings.Contains(w.Body.String(), "/disconnect") {
		t.Error("Trennen while not connected")
	}
	fc.v = Connection{ID: "nextcloud", Title: "Nextcloud", Connected: true, Login: "alice", Since: time.Date(2026, 10, 17, 9, 0, 0, 0, time.Local)}
	wantBody(t, do(t, h, req{path: "/connections"}), "als <strong>alice</strong> seit 17.10.2026", "Neu verbinden", `action="/connections/nextcloud/disconnect"`)
	fc.v.Expired = true
	wantBody(t, do(t, h, req{path: "/connections"}), "Verbindung abgelaufen", "Neu verbinden")
	fc.v = Connection{ID: "nextcloud", Title: "Nextcloud", Flow: Flow{State: FlowMismatch, Ended: time.Now()}}
	wantBody(t, do(t, h, req{path: "/connections"}), "Fehler", "Bitte mit deinem eigenen Nextcloud-Konto verbinden.")
	// an old failure is not shown any more
	fc.v.Flow.Ended = time.Now().Add(-2 * time.Hour)
	if w := do(t, h, req{path: "/connections"}); strings.Contains(w.Body.String(), "eigenen Nextcloud-Konto") {
		t.Error("stale failure shown")
	}
}

func TestConnectFlow(t *testing.T) {
	h, fc, _ := setupConns(t)
	// no attempt yet: the waiting page sends back to the list
	w := do(t, h, req{path: "/connections/nextcloud"})
	wantStatus(t, w, http.StatusSeeOther)

	// cross-site form posts never start a login
	w = do(t, h, req{method: "POST", path: "/connections/nextcloud/connect", form: url.Values{}, header: map[string]string{"Origin": "https://evil.example"}})
	wantStatus(t, w, http.StatusForbidden)
	w = do(t, h, req{method: "POST", path: "/connections/nextcloud/disconnect", form: url.Values{}, header: map[string]string{"Sec-Fetch-Site": "cross-site"}})
	wantStatus(t, w, http.StatusForbidden)
	if len(fc.connects) != 0 || fc.disconnects != 0 {
		t.Fatal("foreign origin reached the core")
	}

	w = do(t, h, req{method: "POST", path: "/connections/nextcloud/connect", form: url.Values{"from": {"onboarding"}}})
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/connections/nextcloud?from=onboarding" || len(fc.connects) != 1 || fc.connects[0] != "anna|nextcloud" {
		t.Fatalf("%s %v", loc, fc.connects)
	}
	w = do(t, h, req{path: "/connections/nextcloud?from=onboarding"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, `href="https://cloud.example.org/login/v2/flow/abc" target="_blank" rel="noopener noreferrer"`, `hx-trigger="every 2s"`,
		"Zugriff gewähren", `href="/onboarding?step=2"`)

	fc.v.Flow.State, fc.v.Flow.Ended, fc.v.Connected, fc.v.Login = FlowDone, time.Now(), true, "anna"
	w = do(t, h, req{path: "/connections/nextcloud?from=onboarding"})
	wantBody(t, w, "Verbunden als <strong>anna</strong>", `href="/onboarding?step=3"`)
	if strings.Contains(w.Body.String(), "hx-trigger") {
		t.Error("still polling after the grant")
	}
	for st, text := range map[FlowState]string{FlowMismatch: "eigenen Nextcloud-Konto", FlowTimeout: "20&nbsp;Minuten", FlowFailed: "kam nicht zustande"} {
		fc.v.Flow = Flow{State: st, Ended: time.Now()}
		w = do(t, h, req{path: "/connections/nextcloud"})
		wantBody(t, w, text, `action="/connections/nextcloud/connect"`)
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/connections/outline/connect", form: url.Values{}}), http.StatusNotFound)

	w = do(t, h, req{method: "POST", path: "/connections/nextcloud/disconnect", form: url.Values{}})
	wantStatus(t, w, http.StatusSeeOther)
	if w.Header().Get("Location") != "/connections?disconnected=nextcloud" || fc.disconnects != 1 {
		t.Fatal(w.Header())
	}
	wantBody(t, do(t, h, req{path: "/connections?disconnected=nextcloud"}), "Nextcloud ist getrennt.")

	fc.err = ErrUnavailable
	w = do(t, h, req{method: "POST", path: "/connections/nextcloud/disconnect", form: url.Values{}})
	wantStatus(t, w, http.StatusBadGateway)
	wantBody(t, w, "Nextcloud ist gerade nicht erreichbar", "nichts geändert")
}

func TestOnboarding(t *testing.T) {
	h, _, fo := setupConns(t)
	// first visit: once to the Onboarding, then the start page
	w := do(t, h, req{path: "/"})
	wantStatus(t, w, http.StatusSeeOther)
	if w.Header().Get("Location") != "/onboarding" || !fo.seen[anna.UID] {
		t.Fatal(w.Header())
	}
	wantStatus(t, do(t, h, req{path: "/"}), http.StatusOK)
	// a start page link with a query (template) never detours
	fo.seen = map[string]bool{}
	wantStatus(t, do(t, h, req{path: "/?template=research"}), http.StatusOK)

	w = do(t, h, req{path: "/onboarding"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, `aria-current="step">Was der Helfer kann`, `href="/onboarding?step=2"`, ">Überspringen<")
	w = do(t, h, req{path: "/onboarding?step=2"})
	wantBody(t, w, `aria-current="step">Verbindungen herstellen`, `name="from" value="onboarding"`, "nicht verbunden")
	w = do(t, h, req{path: "/onboarding?step=9"})
	wantBody(t, w, `aria-current="step">Erste Aufgabe`, `href="/?template=text"`)
	if n := strings.Count(w.Body.String(), "<h1"); n != 1 {
		t.Errorf("%d h1", n)
	}
	for _, p := range []string{"/environment", "/help"} {
		wantBody(t, do(t, h, req{path: p}), `href="/onboarding"`, `href="/connections"`)
	}
}
