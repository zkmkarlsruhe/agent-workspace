// SPDX-License-Identifier: MIT

package portal

import (
	"archive/zip"
	"bytes"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestExport(t *testing.T) {
	h, f := setup(t)
	f.addProject("bericht", "")
	s := time.Now().Format("2006-01-02")

	w := do(t, h, req{path: "/environment/export"})
	wantStatus(t, w, 200)
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename=workspace-data-`+s+`.zip` {
		t.Fatalf("disposition %q", cd)
	}
	if _, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len())); err != nil || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("not a zip: %v", err)
	}
	w = do(t, h, req{path: "/projects/bericht/export"})
	wantStatus(t, w, 200)
	if !strings.Contains(w.Header().Get("Content-Disposition"), "bericht-export-"+s+".zip") {
		t.Fatal(w.Header().Get("Content-Disposition"))
	}
	if !slices.Equal(f.calls, []string{"export:", "export:bericht"}) {
		t.Fatalf("calls %v", f.calls)
	}
	wantStatus(t, do(t, h, req{path: "/projects/fehlt/export"}), 404)
	wantStatus(t, do(t, h, req{path: "/projects/..%2Fx/export"}), 404)

	// an error before the first byte is a German page, not a broken download
	f.exportErr = ErrNotRunning
	w = do(t, h, req{path: "/environment/export"})
	wantStatus(t, w, 503)
	if w.Header().Get("Content-Disposition") != "" {
		t.Fatal("headers of a download on an error page")
	}
	wantBody(t, w, "Deine Umgebung läuft nicht")

	// links: Umgebung, Hilfe, project page next to the ZIP
	f.exportErr = nil
	wantBody(t, do(t, h, req{path: "/environment"}), `href="/environment/export"`, "Meine Daten exportieren", `href="/environment/delete"`)
	wantBody(t, do(t, h, req{path: "/help"}), `href="/environment/export"`)
	wantBody(t, do(t, h, req{path: "/projects/bericht"}), `href="/projects/bericht/export"`, "Projekt exportieren")
}

func TestDeleteAccount(t *testing.T) {
	h, f := setup(t)
	w := do(t, h, req{path: "/environment/delete"})
	wantStatus(t, w, 200)
	wantBody(t, w, "Was gelöscht wird", `href="/environment/export"`, `name="confirmation"`, "LÖSCHEN")

	for _, word := range []string{"", "loeschen", "ja", "LÖSCHEN!"} {
		w := do(t, h, req{method: "POST", path: "/environment/delete", form: url.Values{"confirmation": {word}}})
		wantStatus(t, w, 422)
		wantBody(t, w, `aria-invalid="true"`, "Bitte gib zur Bestätigung das Wort LÖSCHEN ein.")
	}
	if f.deleted != 0 {
		t.Fatal("deleted without the word")
	}
	// CSRF: a preview host or another site cannot delete, even with the word
	for _, hd := range []map[string]string{
		{"Sec-Fetch-Site": "same-site", "Origin": "https://anna--3000.apps.example.org"},
		{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"},
		{"Origin": "https://evil.example"},
	} {
		w := do(t, h, req{method: "POST", path: "/environment/delete", form: url.Values{"confirmation": {"LÖSCHEN"}}, header: hd})
		wantStatus(t, w, 403)
	}
	if f.deleted != 0 {
		t.Fatal("cross-origin request deleted")
	}
	wantStatus(t, do(t, h, req{path: "/environment/delete", method: "GET", form: nil}), 200) // GET never deletes
	if f.deleted != 0 {
		t.Fatal("GET deleted")
	}

	f.deleteErr = ErrUnavailable
	w = do(t, h, req{method: "POST", path: "/environment/delete", form: url.Values{"confirmation": {"LÖSCHEN"}}})
	wantStatus(t, w, 503)
	wantBody(t, w, "Gerade nicht möglich")

	f.deleteErr = nil
	w = do(t, h, req{method: "POST", path: "/environment/delete", form: url.Values{"confirmation": {" löschen "}},
		header: map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://apps.example.org"}})
	wantStatus(t, w, 303)
	if w.Header().Get("Location") != "https://anna.apps.example.org/__workspace/clear" || f.deleted != 2 {
		t.Fatalf("location %q, %d calls", w.Header().Get("Location"), f.deleted)
	}
	wantBody(t, do(t, h, req{path: "/deleted"}), "Deine Daten sind gelöscht", "leeren Konto")
}

func TestDeleteAccountDropsPending(t *testing.T) {
	var p pendingStore
	now := time.Now()
	a := p.add(anna.UID, "x", "geheimer Auftrag", now)
	b := p.add(strings.Repeat("b", 64), "x", "anderer", now)
	p.forget(anna.UID)
	if p.get(a, anna.UID, "x", now) != nil || p.get(b, strings.Repeat("b", 64), "x", now) == nil {
		t.Fatal("forget")
	}
}

func TestInactivityHints(t *testing.T) {
	h, f := setup(t)
	f.addProject("bald", "")
	f.addProject("spaeter", "")
	f.projects["bald"].TrashAt = time.Now().Add(6 * 24 * time.Hour)
	f.projects["spaeter"].TrashAt = time.Now().Add(8 * 24 * time.Hour)
	day := f.projects["bald"].TrashAt.Local().Format("02.01.2006")
	w := do(t, h, req{path: "/"})
	if n := strings.Count(w.Body.String(), "in den Papierkorb gelegt"); n != 1 {
		t.Fatalf("%d hints on the start page", n)
	}
	wantBody(t, w, "Ohne Nutzung wird es am "+day+" in den Papierkorb gelegt.")
	wantBody(t, do(t, h, req{path: "/projects/bald"}), "wird am "+day+" in den Papierkorb gelegt")
	if strings.Contains(do(t, h, req{path: "/projects/spaeter"}).Body.String(), "in den Papierkorb gelegt") {
		t.Fatal("hint more than 7 days ahead")
	}
	wantBody(t, do(t, h, req{path: "/help"}),
		"Projekte, die 30&nbsp;Tage nicht benutzt werden, wandern in den Papierkorb; dort werden sie nach weiteren 30&nbsp;Tagen gelöscht.")
	f.trash = []TrashItem{{ID: "t1", Name: "alt", Auto: true}, {ID: "t2", Name: "selbst"}}
	w = do(t, h, req{path: "/trash"})
	if n := strings.Count(w.Body.String(), "automatisch nach 30&nbsp;Tagen ohne Nutzung"); n != 1 {
		t.Fatalf("%d auto reasons", n)
	}
}
