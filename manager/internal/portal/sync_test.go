// SPDX-License-Identifier: MIT

package portal

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeSync struct {
	connected bool
	st        SyncStatus
	calls     []string
}

func (f *fakeSync) Connected(context.Context, string) bool { return f.connected }
func (f *fakeSync) Status(_ context.Context, _, project string) (SyncStatus, error) {
	s := f.st
	s.Connected = f.connected
	return s, nil
}
func (f *fakeSync) Link(_ context.Context, _, project string, all bool) error {
	if !f.connected {
		return ErrNotConnected
	}
	f.calls = append(f.calls, "link|"+project+"|"+map[bool]string{true: "alles", false: "results"}[all])
	f.st = SyncStatus{Linked: true, Folder: "Agent Workspace/" + project, URL: "https://cloud.example/apps/files/?dir=x", Last: time.Now()}
	return nil
}
func (f *fakeSync) Unlink(_ context.Context, _, project string) error {
	f.calls = append(f.calls, "unlink|"+project)
	f.st.Linked = false
	return nil
}
func (f *fakeSync) Pull(_ context.Context, _, project string) (PullResult, error) {
	f.calls = append(f.calls, "pull|"+project)
	return PullResult{Updated: []string{"results/a.pptx"}, Conflicts: []string{"inbox/b (Nextcloud).docx"}}, nil
}
func (f *fakeSync) AddCollection(_ context.Context, _, project, path string) error {
	if path == "fehlt" {
		return ErrNotFound
	}
	f.calls = append(f.calls, "col|"+path)
	f.st.Collections = append(f.st.Collections, Collection{Path: path, URL: "https://cloud.example/x"})
	return nil
}
func (f *fakeSync) RemoveCollection(_ context.Context, _, project, path string) error {
	f.calls = append(f.calls, "uncol|"+path)
	f.st.Collections = nil
	return nil
}

func TestSync(t *testing.T) {
	f := newFake()
	f.addProject("bericht", "Zahlen 2025 auswerten")
	fa := &fakeSync{}
	h, err := New(Config{Backend: f, Sync: fa, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, h, req{path: "/projects/bericht"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, `id="nextcloud"`, `href="/connections"`)
	if strings.Contains(w.Body.String(), "/nextcloud/verknuepfen") {
		t.Fatal("link offered without a Nextcloud login")
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/projects/bericht/nextcloud/link", form: url.Values{}}), http.StatusSeeOther)
	if len(fa.calls) != 0 {
		t.Fatal(fa.calls)
	}

	fa.connected = true
	wantBody(t, do(t, h, req{path: "/projects/bericht"}), `action="/projects/bericht/nextcloud/link"`, ">In Nextcloud ablegen<", "Sammlung verknüpfen")

	// CSRF: a cross-site post never links
	for _, hd := range []map[string]string{{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, {"Origin": "https://anna--3000.apps.example.org"}} {
		wantStatus(t, do(t, h, req{method: "POST", path: "/projects/bericht/nextcloud/link", form: url.Values{"all": {"1"}}, header: hd}), http.StatusForbidden)
	}
	if len(fa.calls) != 0 {
		t.Fatalf("cross-site post reached the core: %v", fa.calls)
	}

	w = do(t, h, req{method: "POST", path: "/projects/bericht/nextcloud/link", form: url.Values{"all": {"1"}}})
	wantStatus(t, w, http.StatusSeeOther)
	if w.Header().Get("Location") != "/projects/bericht?nc=linked#nextcloud" || fa.calls[0] != "link|bericht|alles" {
		t.Fatalf("%s %v", w.Header().Get("Location"), fa.calls)
	}
	w = do(t, h, req{path: "/projects/bericht?nc=linked"})
	wantBody(t, w, "Das Projekt ist mit Nextcloud verknüpft.", "Verknüpft mit Nextcloud: <a href=\"https://cloud.example/apps/files/?dir=x\"", "Agent Workspace/bericht/</a>",
		"zuletzt übertragen heute", ">Aus Nextcloud holen<", ">Verknüpfung lösen<")

	w = do(t, h, req{method: "POST", path: "/projects/bericht/nextcloud/pull", form: url.Values{}})
	loc := w.Header().Get("Location")
	wantBody(t, do(t, h, req{path: strings.TrimSuffix(loc, "#nextcloud")}), "Aus Nextcloud geholt: 1 Datei aktualisiert.", "1 Datei wurde auch vom Helfer geändert")

	w = do(t, h, req{method: "POST", path: "/projects/bericht/collections", form: url.Values{"path": {" Team/Presse "}}})
	if w.Header().Get("Location") != "/projects/bericht?nc=collection-added#nextcloud" || fa.calls[len(fa.calls)-1] != "col|Team/Presse" {
		t.Fatalf("%s %v", w.Header().Get("Location"), fa.calls)
	}
	wantBody(t, do(t, h, req{path: "/projects/bericht"}), ">Team/Presse</a>", `name="path" value="Team/Presse"`)
	if w := do(t, h, req{method: "POST", path: "/projects/bericht/collections", form: url.Values{"path": {"fehlt"}}}); w.Header().Get("Location") != "/projects/bericht?nc=collection-missing#nextcloud" {
		t.Fatal(w.Header().Get("Location"))
	}
	do(t, h, req{method: "POST", path: "/projects/bericht/collections/remove", form: url.Values{"path": {"Team/Presse"}}})
	do(t, h, req{method: "POST", path: "/projects/bericht/nextcloud/unlink", form: url.Values{}})
	if strings.Join(fa.calls[len(fa.calls)-2:], ",") != "uncol|Team/Presse,unlink|bericht" {
		t.Fatal(fa.calls)
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/projects/..%2Fx/nextcloud/unlink", form: url.Values{}}), http.StatusNotFound)

	// Start offers the link when connected and links the new project
	wantBody(t, do(t, h, req{path: "/"}), `name="nextcloud" value="1" checked`)
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("description", "Eine Präsentation zur Ausstellung bauen")
	mw.WriteField("nextcloud", "1")
	mw.Close()
	wantStatus(t, do(t, h, req{method: "POST", path: "/projects/start", body: &b, ctype: mw.FormDataContentType()}), http.StatusSeeOther)
	if last := fa.calls[len(fa.calls)-1]; !strings.HasPrefix(last, "link|") || !strings.HasSuffix(last, "|results") {
		t.Fatalf("loslegen did not link: %v", fa.calls)
	}
	fa.connected = false
	if strings.Contains(do(t, h, req{path: "/"}).Body.String(), `name="nextcloud"`) {
		t.Fatal("offered without login")
	}
}

func TestSyncHiddenWithout(t *testing.T) {
	h, f := setup(t)
	f.addProject("bericht", "")
	if strings.Contains(do(t, h, req{path: "/projects/bericht"}).Body.String(), `id="nextcloud"`) {
		t.Fatal("section without Nextcloud")
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/projects/bericht/nextcloud/link", form: url.Values{}}), http.StatusNotFound)
}
