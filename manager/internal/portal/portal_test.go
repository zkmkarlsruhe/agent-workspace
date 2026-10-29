// SPDX-License-Identifier: MIT

package portal

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/instance"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var anna = Identity{UID: strings.Repeat("a", 64), Username: "anna", Name: "Anna Beispiel", Groups: []string{"authentik Admins", UserGroup}}

func setup(t *testing.T) (http.Handler, *fake) {
	t.Helper()
	f := newFake()
	h, err := New(Config{Backend: f, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org", Group: UserGroup, UploadMaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return h, f
}

type req struct {
	method, path string
	form         url.Values
	body         io.Reader
	ctype        string
	header       map[string]string
	id           *Identity
}

func do(t *testing.T, h http.Handler, q req) *httptest.ResponseRecorder {
	t.Helper()
	if q.method == "" {
		q.method = http.MethodGet
	}
	body := q.body
	if q.form != nil {
		body = strings.NewReader(q.form.Encode())
		q.ctype = "application/x-www-form-urlencoded"
	}
	r := httptest.NewRequest(q.method, "https://apps.example.org"+q.path, body)
	if q.ctype != "" {
		r.Header.Set("Content-Type", q.ctype)
	}
	for k, v := range q.header {
		r.Header.Set(k, v)
	}
	id := anna
	if q.id != nil {
		id = *q.id
	}
	r = r.WithContext(WithIdentity(r.Context(), id))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status %d, want %d; body:\n%s", w.Code, code, w.Body.String())
	}
}

func wantBody(t *testing.T, w *httptest.ResponseRecorder, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(w.Body.String(), s) {
			t.Errorf("body lacks %q", s)
		}
	}
}

func TestNoAccess(t *testing.T) {
	h, _ := setup(t)
	for _, id := range []Identity{{}, {UID: anna.UID, Groups: []string{"andere"}}} {
		w := do(t, h, req{path: "/", id: &id})
		wantStatus(t, w, http.StatusForbidden)
		wantBody(t, w, "Kein Zugang")
	}
	// Static assets need no identity (the error page uses them).
	w := do(t, h, req{path: "/static/js/htmx.min.js", id: &Identity{}})
	wantStatus(t, w, http.StatusOK)
}

func TestSecurityHeaders(t *testing.T) {
	h, f := setup(t)
	f.addProject("demo", "Beschreibung")
	w := do(t, h, req{path: "/"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, `lang="de"`, "demo", "Zum Inhalt", "Anna Beispiel")
	for _, k := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy"} {
		if w.Header().Get(k) == "" {
			t.Errorf("missing %s", k)
		}
	}
	// Chrome checks form-action on the 303 to <slug>.apps.example.org ("Öffnen", "Loslegen")
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://*.apps.example.org;") {
		t.Errorf("form-action must allow the person hosts: %s", csp)
	}
	if strings.Contains(w.Body.String(), "style=") {
		t.Error("inline style breaks CSP")
	}
}

func TestCSRF(t *testing.T) {
	h, f := setup(t)
	f.addProject("x", "")
	cases := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"preview host same-site", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://anna--3000.apps.example.org"}, 403},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, 403},
		{"old browser, foreign origin", map[string]string{"Origin": "https://anna--3000.apps.example.org"}, 403},
		{"same origin", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://apps.example.org"}, 303},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, h, req{method: "POST", path: "/projects/x/delete", form: url.Values{}, header: c.header})
			wantStatus(t, w, c.want)
		})
	}
	if len(f.trash) != 1 {
		t.Fatalf("only the same-origin request may delete, trash=%v", f.trash)
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"Besucherzahlen Auswertung 2025": "besucherzahlen-auswertung-2025",
		"  Größe -- über ÄÖÜ!! ":         "groesse-ueber-aeoeue",
		"papierkorb":                     "",
		strings.Repeat("wort ", 20):      "wort-wort-wort-wort-wort-wort-wort-wort-wort-wort",
	} {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
	for n, ok := range map[string]bool{"abc-1": true, "-abc": false, "ABC": false, "a/b": false, "": false, strings.Repeat("a", 51): false, strings.Repeat("a", 50): true, "papierkorb": false, "..": false} {
		if (validName(n) == "") != ok {
			t.Errorf("validName(%q) ok=%v", n, !ok)
		}
	}
	now := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	if got := fallbackName("Werte bitte die Tabelle aus", now); got != "2026-10-16-werte-bitte-die" {
		t.Errorf("fallback %q", got)
	}
}

func TestStartFlow(t *testing.T) {
	h, f := setup(t)
	f.env.State = EnvStopped
	f.suggest = "text"
	desc := "Schreibe einen Pressetext."
	body, ct := taskBody(t, desc, "")
	w := do(t, h, req{method: "POST", path: "/projects/start", body: body, ctype: ct})
	wantStatus(t, w, 303)
	wait := w.Header().Get("Location")
	tok := strings.TrimPrefix(wait, "/environment/starting?project=text&t=")
	if !tokenRE.MatchString(tok) {
		t.Fatalf("Location %q", wait)
	}
	if len(f.calls) != 1 || f.calls[0] != "start" {
		t.Fatalf("calls %v", f.calls)
	}

	w = do(t, h, req{path: wait})
	wantStatus(t, w, 200)
	wantBody(t, w, "hx-trigger=\"every 1s", `http-equiv="refresh"`, "Deine Umgebung startet")

	f.env.State = EnvRunning
	w = do(t, h, req{path: wait})
	wantBody(t, w, `hx-post="/projects/text/open"`, `hx-trigger="load"`, `name="t" value="`+tok+`"`)

	// the auto-post from htmx gets HX-Redirect, not a cross-origin 303
	open := func(hx bool) *httptest.ResponseRecorder {
		hdr := map[string]string{}
		if hx {
			hdr["HX-Request"] = "true"
		}
		return do(t, h, req{method: "POST", path: "/projects/text/open", form: url.Values{"t": {tok}}, header: hdr})
	}
	w = open(true)
	wantStatus(t, w, 200)
	link := w.Header().Get("HX-Redirect")
	if !strings.HasPrefix(link, "https://anna.apps.example.org/") {
		t.Errorf("HX-Redirect %q", link)
	}
	// reload of the waiting page (auto-post again) and the no-JS button: same chat, no second run
	if w = open(true); w.Header().Get("HX-Redirect") != link {
		t.Errorf("second post: %q", w.Header().Get("HX-Redirect"))
	}
	if w = open(false); w.Code != 303 || w.Header().Get("Location") != link {
		t.Errorf("no-JS post: %d %q", w.Code, w.Header().Get("Location"))
	}
	if len(f.sessions) != 1 || f.sessions[0] != "text|"+desc {
		t.Errorf("sessions %v", f.sessions)
	}
	// unknown or other user's token: no session, back to the project
	other := Identity{UID: strings.Repeat("b", 64), Groups: []string{UserGroup}}
	w = do(t, h, req{method: "POST", path: "/projects/text/open", form: url.Values{"t": {tok}}, id: &other})
	if w.Header().Get("Location") != "/projects/text" || len(f.sessions) != 1 {
		t.Errorf("foreign token: %q %v", w.Header().Get("Location"), f.sessions)
	}
	w = do(t, h, req{method: "POST", path: "/projects/text/open", form: url.Values{"t": {strings.Repeat("0", 32)}}})
	if w.Header().Get("Location") != "/projects/text" || len(f.sessions) != 1 {
		t.Errorf("unknown token: %q %v", w.Header().Get("Location"), f.sessions)
	}
	// waiting page without a valid token never auto-posts
	if strings.Contains(do(t, h, req{path: "/environment/starting?project=text"}).Body.String(), "oeffnen") {
		t.Error("auto-post without token")
	}

	f.env = EnvStatus{State: EnvError, ErrorID: "U-7F3A"}
	w = do(t, h, req{path: "/environment/starting"})
	wantStatus(t, w, 503)
	wantBody(t, w, "konnte nicht starten", "U-7F3A", "Erneut versuchen")
	w = do(t, h, req{path: "/environment/starting", header: map[string]string{"HX-Request": "true"}})
	if w.Header().Get("HX-Refresh") != "true" {
		t.Error("poller must reload on error")
	}
}

func TestDownload(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "")
	f.files["p/results/bericht ä.html"] = []byte("<script>alert(1)</script>")
	w := do(t, h, req{path: "/projects/p/files/results/bericht%20%C3%A4.html"})
	wantStatus(t, w, 200)
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "filename*=utf-8''bericht%20%C3%A4.html") {
		t.Errorf("Content-Disposition %q", cd)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff missing")
	}
	for _, p := range []string{
		"/projects/p/files/../../etc/passwd",
		"/projects/p/files/results/%2e%2e/%2e%2e/x",
		"/projects/p/files/.git/config",
		"/projects/p/files/work/.opencode/x",
		"/projects/p/files/work/%2eenv",
		"/projects/p/files/results/..%2f..%2fx",
		"/projects/p/files//etc/passwd",
		"/projects/..%2fq/files/x",
	} {
		w := do(t, h, req{path: p})
		if w.Code != 404 && w.Code != 301 && w.Code != 307 {
			t.Errorf("%s: status %d", p, w.Code)
		}
	}
	for _, p := range []string{"../x", "/abs", "a/../../b", "a/.hidden", "a\\b", "ok/../.git/x"} {
		if _, ok := cleanRelPath(p); ok {
			t.Errorf("cleanRelPath(%q) accepted", p)
		}
	}
}

func TestPDF(t *testing.T) {
	f := newFake()
	f.addProject("p", "")
	f.files["p/results/bericht ä.md"] = []byte("# Bericht")
	f.tree = []File{{Name: "results", Path: "results", Dir: true}, {Name: "bericht ä.md", Path: "results/bericht ä.md"}, {Name: "daten.csv", Path: "results/daten.csv"}}
	var got string
	h, err := New(Config{Backend: f, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org",
		PDF: func(_ context.Context, md []byte, name string) ([]byte, error) {
			got = string(md) + "|" + name
			return []byte("%PDF-fake"), nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, h, req{path: "/projects/p"})
	wantStatus(t, w, 200)
	wantBody(t, w, `href="/projects/p/pdf?file=results%2Fbericht&#43;%C3%A4.md"`, ">Als PDF<")
	if strings.Count(w.Body.String(), ">Als PDF<") != 1 {
		t.Error("Als PDF only for .md files")
	}

	w = do(t, h, req{path: "/projects/p/pdf?file=results%2Fbericht+%C3%A4.md"})
	wantStatus(t, w, 200)
	if w.Body.String() != "%PDF-fake" || got != "# Bericht|bericht ä.md" || w.Header().Get("Content-Type") != "application/pdf" {
		t.Errorf("body %q md %q ctype %q", w.Body, got, w.Header().Get("Content-Type"))
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, "bericht%20%C3%A4.pdf") {
		t.Errorf("Content-Disposition %q", cd)
	}
	for _, q := range []string{"../x.md", ".project/brief.md", "results/daten.csv", "/etc/x.md", "results/fehlt.md", ""} {
		if w := do(t, h, req{path: "/projects/p/pdf?" + url.Values{"datei": {q}}.Encode()}); w.Code != 404 {
			t.Errorf("%q: status %d", q, w.Code)
		}
	}

	h, _ = setup(t) // no OC_PDF_URL: no link, no route
	w = do(t, h, req{path: "/projects/p"})
	if strings.Contains(w.Body.String(), "Als PDF") {
		t.Error("Als PDF shown without converter")
	}
}

func TestZip(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "")
	w := do(t, h, req{path: "/projects/p/zip?part=all"})
	wantStatus(t, w, 200)
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "p-alles-") {
		t.Errorf("Content-Disposition %q", cd)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil || zr.File[0].Name != "all/x.txt" {
		t.Fatalf("zip: %v", err)
	}
	wantStatus(t, do(t, h, req{path: "/projects/fehlt/zip"}), 404)
}

func multipartBody(t *testing.T, files map[string]string) (io.Reader, string) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for name, content := range files {
		fw, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write([]byte(content))
	}
	mw.Close()
	return &b, mw.FormDataContentType()
}

func TestUpload(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "")
	body, ct := multipartBody(t, map[string]string{"../../.bashrc": "x", "C:\\Users\\anna\\daten.xlsx": "y"})
	w := do(t, h, req{method: "POST", path: "/projects/p/upload", body: body, ctype: ct})
	wantStatus(t, w, 303)
	if w.Header().Get("Location") != "/projects/p?uploaded=2#inbox" {
		t.Errorf("Location %q", w.Header().Get("Location"))
	}
	if _, ok := f.files["p/inbox/bashrc"]; !ok {
		t.Errorf("files %v", f.files)
	}
	if _, ok := f.files["p/inbox/daten.xlsx"]; !ok {
		t.Errorf("files %v", f.files)
	}

	body, ct = multipartBody(t, map[string]string{"gross.bin": strings.Repeat("x", 2<<20)})
	w = do(t, h, req{method: "POST", path: "/projects/p/upload", body: body, ctype: ct})
	wantStatus(t, w, 413)
	wantBody(t, w, "zu groß", `id="inbox"`, "files-error")
}

func TestTrash(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "")
	f.busy["p"] = true
	w := do(t, h, req{method: "POST", path: "/projects/p/delete", form: url.Values{}})
	wantStatus(t, w, 409)
	wantBody(t, w, "arbeitet gerade")
	f.busy["p"] = false

	wantBody(t, do(t, h, req{path: "/projects/p/delete"}), "In den Papierkorb", "dialog--inline")
	w = do(t, h, req{method: "POST", path: "/projects/p/delete", form: url.Values{}})
	wantStatus(t, w, 303)
	wantBody(t, do(t, h, req{path: "/trash"}), "p--1", "Wiederherstellen")

	// name taken: ask for a new one
	f.addProject("p", "")
	w = do(t, h, req{method: "POST", path: "/trash/p--1/restore", form: url.Values{}})
	wantStatus(t, w, 409)
	wantBody(t, w, `id="new_name"`)
	w = do(t, h, req{method: "POST", path: "/trash/p--1/restore", form: url.Values{"new_name": {"p-alt"}}})
	wantStatus(t, w, 303)
	if w.Header().Get("Location") != "/projects/p-alt?restored=1" {
		t.Errorf("Location %q", w.Header().Get("Location"))
	}

	// purge needs the confirmation field
	f.MoveToTrash(nil, "", "p")
	w = do(t, h, req{method: "POST", path: "/trash/p--1/delete", form: url.Values{}})
	wantStatus(t, w, 422)
	w = do(t, h, req{method: "POST", path: "/trash/p--1/delete", form: url.Values{"confirmed": {"yes"}}})
	wantStatus(t, w, 303)
	if len(f.trash) != 0 {
		t.Errorf("trash %v", f.trash)
	}
	wantStatus(t, do(t, h, req{path: "/trash/nicht-da/delete"}), 404)
}

func TestEnvironment(t *testing.T) {
	h, f := setup(t)
	f.env = EnvStatus{State: EnvRunning, Since: time.Now(), DiskUsed: 7_200_000_000, DiskQuota: 20 << 30, ActiveRuns: []string{"ses_b1"}}
	w := do(t, h, req{path: "/environment"})
	wantBody(t, w, "läuft", "6,7&nbsp;GB von 20,0&nbsp;GB", "<dd>1</dd>", "bleiben immer erhalten")

	w = do(t, h, req{path: "/environment/reset"})
	wantBody(t, w, "Bleibt erhalten", "Projekte und Dateien", "Geht verloren", `name="understood"`)

	w = do(t, h, req{method: "POST", path: "/environment/reset", form: url.Values{}})
	wantStatus(t, w, 422)
	wantBody(t, w, "understood-error")
	if len(f.calls) != 0 {
		t.Fatalf("reset without confirmation: %v", f.calls)
	}
	w = do(t, h, req{method: "POST", path: "/environment/reset", form: url.Values{"understood": {"yes"}}})
	wantStatus(t, w, 303)
	w = do(t, h, req{method: "POST", path: "/environment/stop", form: url.Values{"confirmed": {"yes"}}})
	wantStatus(t, w, 303)
	if strings.Join(f.calls, ",") != "reset,stop" {
		t.Errorf("calls %v", f.calls)
	}
	wantStatus(t, do(t, h, req{method: "POST", path: "/environment/sprengen", form: url.Values{}}), 404)
}

func TestPagesRender(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "Beschreibung")
	f.projects["p"].CostEUR, f.projects["p"].CO2MinG, f.projects["p"].CO2MaxG = 0.42, 5.83, 25
	f.projects["p"].ChatList = []Chat{{ID: "ses_1", Title: "Erster Chat", Status: StatusWaiting, CostEUR: 0.3, CO2MinG: 0.04, CO2MaxG: 0.16,
		Link: "https://anna.apps.example.org/server/x/session/ses_1"}}
	f.usage = []UsageRow{{Project: "p", CostEUR: 1234.5, CO2MinG: 6, CO2MaxG: 23, Chats: 3}, {Project: "q", CostEUR: 0}}
	for path, want := range map[string]string{
		"/":             "Meine Projekte",
		"/projects/p":   "wartet auf deine Antwort",
		"/usage":        "1.234,50\u00a0€",
		"/usage/method": "Faktor 4 und mehr",
		"/help":         "Was steht auf der Seite eines Projekts?",
		"/privacy":      "Datenschutzhinweis",
		"/trash":        "Der Papierkorb ist leer",
		"/gibtsnicht":   "Seite nicht gefunden",
	} {
		w := do(t, h, req{path: path})
		wantBody(t, w, want)
		if n := strings.Count(w.Body.String(), "<h1"); n != 1 {
			t.Errorf("%s: %d h1", path, n)
		}
	}
	wantBody(t, do(t, h, req{path: "/projects/p"}), `href="https://anna.apps.example.org/server/x/session/ses_1"`, "Noch keine Dateien")
	wantBody(t, do(t, h, req{path: "/usage"}), "ca. 6–20\u00a0g", `value="100"`, `href="/usage/method"`)
	wantBody(t, do(t, h, req{path: "/projects"}), "0,42\u00a0€ &middot; 6–30\u00a0g CO<sub>2</sub> (30&nbsp;Tage)")
	wantBody(t, do(t, h, req{path: "/projects/p"}), "0,30\u00a0€ &middot; 0,04–0,2\u00a0g CO<sub>2</sub>", "0,42\u00a0€ &middot; 6–30\u00a0g")
	wantBody(t, do(t, h, req{path: "/usage/method"}), instance.Current().Models.Thorough, instance.Current().Footprint["thorough"].Name, "sehr grobe Schätzung",
		"5,8", "0,061", "344&nbsp;g", "CC BY-SA 4.0")
	wantStatus(t, do(t, h, req{path: "/projects/fehlt"}), 404)
}

func TestProjectTree(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "")
	f.tree = []File{
		{Name: "zeta.md", Path: "zeta.md", Size: 1200},
		{Name: "work", Path: "work", Dir: true},
		{Name: "unter", Path: "work/unter", Dir: true},
		{Name: "tief ä.txt", Path: "work/unter/tief ä.txt", Size: 5},
		{Name: "results", Path: "results", Dir: true},
		{Name: "Bericht.pdf", Path: "results/Bericht.pdf", Size: 7_200_000},
		{Name: "anhang.csv", Path: "results/anhang.csv", Size: 3},
		{Name: "inbox", Path: "inbox", Dir: true},
		{Name: "waise.txt", Path: "fehlt/waise.txt"},
	}
	f.truncated = true
	w := do(t, h, req{path: "/projects/p"})
	wantStatus(t, w, 200)
	body := w.Body.String()
	wantBody(t, w, `href="/projects/p/files/work/unter/tief%20%C3%A4.txt"`, "6,9\u00a0MB", "Die Listen sind gekürzt", "1 Eintrag")
	if strings.Contains(body, "Noch keine Ergebnisse") || strings.Contains(body, "waise") || strings.Contains(body, ">results/") {
		t.Error("hint, orphan or results/ in the tree shown")
	}
	// results first; in the tree folders first, then files, case-insensitive
	order := []string{">anhang.csv<", ">Bericht.pdf<", ">work/", ">zeta.md<"}
	last := -1
	for _, s := range order {
		i := strings.Index(body, s)
		if i <= last {
			t.Fatalf("order: %q at %d after %d", s, i, last)
		}
		last = i
	}
	if n := strings.Count(body, " open>"); n != 0 {
		t.Errorf("%d open folders", n)
	}

	f.files["p/work/unter/tief ä.txt"] = []byte("hallo")
	w = do(t, h, req{path: "/projects/p/files/work/unter/tief%20%C3%A4.txt"})
	wantStatus(t, w, 200)
	wantBody(t, w, "hallo")
}

func oneH1(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if n := strings.Count(w.Body.String(), "<h1"); n != 1 {
		t.Errorf("%s: %d h1", what, n)
	}
}

func TestEmptyStatesAndTemplates(t *testing.T) {
	h, f := setup(t)
	w := do(t, h, req{path: "/"})
	wantBody(t, w, "Noch keine Projekte", `href="/?template=research"`, `href="/?template=table"`, `href="/?template=text"`,
		"Recherche mit Quellen", "Tabelle auswerten", "Text oder Dokument erstellen", "Papierkorb (0)")
	oneH1(t, "/ leer", w)

	for _, v := range append(slices.Clone(taskTemplates), findTemplate("research").in(langDE), findTemplate("table").in(langDE), findTemplate("text").in(langDE)) {
		if d, msg := cleanDescription(v.Description); msg != "" || d != v.Description {
			t.Errorf("template %s: %q", v.ID, msg)
		}
	}
	w = do(t, h, req{path: "/?template=table"})
	wantBody(t, w, ">Werte die Tabelle im \x45ingang aus", "Vorlage „Tabelle auswerten“")
	if strings.Contains(do(t, h, req{path: "/?template=gibtsnicht"}).Body.String(), "Vorlage „") {
		t.Error("unknown template prefilled")
	}

	// empty project, environment running vs. sleeping
	f.addProject("p", "")
	w = do(t, h, req{path: "/projects/p"})
	wantBody(t, w, "Noch keine Chats in diesem Projekt", "Noch keine Dateien")
	f.env.State = EnvStopped
	w = do(t, h, req{path: "/projects/p"})
	wantBody(t, w, "Deine Umgebung schläft", "nicht abrufbar")
	if strings.Contains(w.Body.String(), "Noch keine Chats") {
		t.Error("sleeping environment shown as 0 chats")
	}
	f.projects["p"].ChatList = []Chat{{ID: "ses_1", Title: "Alt"}}
	wantBody(t, do(t, h, req{path: "/projects/p"}), "letzten bekannten Stand", ">Alt<")
}

func TestErrorPagesGerman(t *testing.T) {
	h, f := setup(t)
	for _, c := range []struct {
		err  error
		code int
		want string
	}{
		{ErrBusy, 409, "Der Helfer arbeitet gerade"},
		{ErrNotRunning, 503, "Deine Umgebung läuft nicht"},
		{ErrQuota, 507, "Speicherplatz voll"},
		{ErrNotFound, 404, "Nicht gefunden"},
		{io.ErrUnexpectedEOF, 500, "Fehlerkennung"},
	} {
		f.envErr = c.err
		w := do(t, h, req{method: "POST", path: "/environment/stop", form: url.Values{}})
		wantStatus(t, w, c.code)
		wantBody(t, w, c.want, `lang="de"`)
		oneH1(t, c.want, w)
		if strings.Contains(w.Body.String(), c.err.Error()) {
			t.Errorf("internal error text leaked: %q", c.err)
		}
	}
	for path, want := range map[string]string{
		"/gibtsnicht":           "Seite nicht gefunden",
		"/projects/new/anlegen": "Seite nicht gefunden", // GET on a POST route
		"/projects/fehlt":       "gibt es nicht (mehr)",
		"/projects/Ungueltig":   "Projekt nicht gefunden",
		"/trash/x/delete":       "Nicht im Papierkorb",
	} {
		w := do(t, h, req{path: path})
		wantStatus(t, w, 404)
		wantBody(t, w, want)
		oneH1(t, path, w)
	}
	// htmx callers target a fragment: the error page replaces the main content
	w := do(t, h, req{method: "POST", path: "/projects/fehlt/open", form: url.Values{}, header: map[string]string{"HX-Request": "true"}})
	if w.Header().Get("HX-Retarget") != "#content" || w.Header().Get("HX-Reselect") != "#content" {
		t.Errorf("htmx error not retargeted: %v", w.Header())
	}
}

func TestAllPagesOneH1(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "Beschreibung")
	f.trash = []TrashItem{{ID: "q--1", Name: "q"}}
	for _, st := range []EnvState{EnvRunning, EnvStopped, EnvStarting, EnvError} {
		f.env = EnvStatus{State: st, ErrorID: "U-1"}
		for _, path := range []string{"/", "/?template=table", "/projects/p", "/projects/p/delete", "/trash", "/trash/q--1/delete",
			"/environment", "/environment/starting", "/environment/reset", "/environment/delete", "/deleted", "/usage", "/help", "/privacy"} {
			oneH1(t, string(st)+" "+path, do(t, h, req{path: path}))
		}
	}
	oneH1(t, "kein Zugang", do(t, h, req{path: "/", id: &Identity{}}))
	f.env = EnvStatus{State: EnvError, ErrorID: "U-1"}
	wantBody(t, do(t, h, req{path: "/environment"}), "U-1", "Erneut versuchen")
}

// The project list shows every chat: the newest three directly, older ones folded.
func TestStartChats(t *testing.T) {
	h, f := setup(t)
	f.addProject("viel", "")
	f.addProject("leer", "Tabelle auswerten")
	for i := range 5 {
		f.projects["viel"].Recent = append(f.projects["viel"].Recent,
			Chat{ID: fmt.Sprint("ses_", i), Title: fmt.Sprint("Chat ", i), Link: fmt.Sprint("https://x/session/ses_", i)})
	}
	w := do(t, h, req{path: "/projects"})
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantBody(t, w, `href="https://x/session/ses_0"`, `href="https://x/session/ses_4"`, "Ältere Chats (2)", `name="brief" value="1"`)
	if more := strings.Index(body, "project-row__more"); more < 0 || strings.Index(body, "ses_2") > more || strings.Index(body, "ses_3") < more {
		t.Error("chats 0-2 must come before the fold, 3-4 inside")
	}
}

// --- portal-ux projekt ---

// section returns the part of body from id="<id>" to the next section start.
func section(body, id string) string {
	i := strings.Index(body, `id="`+id+`"`)
	if i < 0 {
		return ""
	}
	rest := body[i:]
	if j := strings.Index(rest, "<section"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func TestProjectPageResultsFirst(t *testing.T) {
	f := newFake()
	f.addProject("p", "")
	f.tree = []File{
		{Name: "work", Path: "work", Dir: true},
		{Name: "skript.py", Path: "work/skript.py", Size: 10},
		{Name: "inbox", Path: "inbox", Dir: true},
		{Name: "liste.xlsx", Path: "inbox/liste.xlsx", Size: 20},
		{Name: "results", Path: "results", Dir: true},
		{Name: "bericht.md", Path: "results/bericht.md", Size: 30},
		{Name: "sub", Path: "results/sub", Dir: true},
		{Name: "daten.csv", Path: "results/sub/daten.csv", Size: 40},
	}
	h, err := New(Config{Backend: f, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org",
		PDF: func(context.Context, []byte, string) ([]byte, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, h, req{path: "/projects/p"})
	wantStatus(t, w, 200)
	oneH1(t, "projekt", w)
	body := w.Body.String()
	resAt, inboxAt, chatsAt := strings.Index(body, `id="results"`), strings.Index(body, `id="inbox"`), strings.Index(body, `id="chats"`)
	if resAt < 0 || inboxAt < resAt || chatsAt < inboxAt || strings.Index(body, ">work/") < chatsAt {
		t.Fatalf("order results %d, inbox %d, chats %d", resAt, inboxAt, chatsAt)
	}
	res := section(body, "results")
	if !strings.Contains(res, `href="/projects/p/files/results/sub/daten.csv" download>sub/daten.csv<`) ||
		!strings.Contains(res, `href="/projects/p/files/results/bericht.md" download>bericht.md<`) {
		t.Errorf("results:\n%s", res)
	}
	if strings.Count(res, ">Als PDF<") != 1 || !strings.Contains(res, `pdf?file=results%2Fbericht.md`) {
		t.Error("Als PDF only for the .md result")
	}
	if !strings.Contains(section(body, "inbox"), `href="/projects/p/files/inbox/liste.xlsx"`) {
		t.Error("inbox file missing")
	}
	if strings.Count(body, "zip?part=results") != 1 || strings.Count(body, "zip?part=all") != 1 {
		t.Error("want one results ZIP and one Alles-ZIP")
	}
	wantBody(t, w, "100\u00a0B", `data-autoupload`, `class="filepick__input"`, `href="/projects/p/delete" data-dialog`)
}

func TestProjectPageEmpty(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "Werte die Tabelle aus.")
	f.projects["p"].SizeBytes = 155648 // folder skeleton, no files
	w := do(t, h, req{path: "/projects/p"})
	wantStatus(t, w, 200)
	body := w.Body.String()
	if strings.Contains(body, "zip?part=") || strings.Contains(body, "152 kB") || strings.Contains(body, "1970") {
		t.Error("empty project shows ZIP, size or a zero date")
	}
	wantBody(t, w, "Noch keine Ergebnisse", "zuerst im Chat", "zuletzt heute", `name="brief" value="1"`, ">Loslegen<",
		"„Neue Aufgabe“ beginnt einen")
	if strings.Contains(body, "Öffnen") || strings.Contains(body, "Neuer Chat") {
		t.Error("old wording")
	}

	f.addProject("q", "")
	w = do(t, h, req{path: "/projects/q"})
	if strings.Contains(w.Body.String(), `name="brief"`) || strings.Count(w.Body.String(), ">Neue Aufgabe<") != 2 {
		t.Error("without description: Neue Aufgabe in head and chats, no Loslegen")
	}

	f.projects["p"].Chats = 2
	f.projects["p"].ChatList = []Chat{
		{ID: "ses_1", Title: "Alt", Updated: time.Now().Add(-time.Hour), Link: "https://x/session/ses_1"},
		{ID: "ses_2", Updated: time.Now(), Link: "https://x/session/ses_2"},
	}
	w = do(t, h, req{path: "/projects/p?restored=1"})
	wantBody(t, w, `href="https://x/session/ses_2">Weiterarbeiten: Ohne Titel<`, `Weiterarbeiten<span class="visually-hidden">: Alt</span>`,
		"Das Projekt „p“ ist wiederhergestellt.")
	if strings.Contains(w.Body.String(), `name="brief"`) {
		t.Error("Loslegen offered next to chats")
	}
}

func TestUploadNextStep(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "Werte die Tabelle aus.")
	inbox := section(do(t, h, req{path: "/projects/p?uploaded=2"}).Body.String(), "inbox")
	for _, s := range []string{"2 Dateien hochgeladen.", `action="/projects/p/open"`, `name="brief" value="1"`, ">Jetzt loslegen<"} {
		if !strings.Contains(inbox, s) {
			t.Errorf("next step without chat: %q missing", s)
		}
	}
	inbox = section(do(t, h, req{path: "/projects/p?uploaded=0"}).Body.String(), "inbox")
	if !strings.Contains(inbox, "Keine Datei ausgewählt.") || strings.Contains(inbox, "loslegen<") {
		t.Error("n=0: message without next step")
	}
	f.projects["p"].Chats = 1
	f.projects["p"].ChatList = []Chat{{ID: "ses_1", Title: "Auswertung", Link: "https://x/session/ses_1"}}
	inbox = section(do(t, h, req{path: "/projects/p?uploaded=1"}).Body.String(), "inbox")
	if !strings.Contains(inbox, "1 Datei hochgeladen.") || !strings.Contains(inbox, `href="https://x/session/ses_1">Weiterarbeiten: Auswertung<`) ||
		strings.Contains(inbox, "loslegen<") {
		t.Errorf("next step with chat:\n%s", inbox)
	}
}

func TestRename(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "")
	f.addProject("belegt", "")
	f.trash = []TrashItem{{ID: "weg--1", Name: "weg"}}
	post := func(n string) *httptest.ResponseRecorder {
		return do(t, h, req{method: "POST", path: "/projects/p/rename", form: url.Values{"new_name": {n}}})
	}
	for n, want := range map[string]string{"belegt": "gibt es schon", "weg": "gibt es schon", "Böse": "Nur Kleinbuchstaben"} {
		w := post(n)
		if w.Code != 409 && w.Code != 422 {
			t.Errorf("%q: status %d", n, w.Code)
		}
		wantBody(t, w, want, `id="new_name-error"`, `aria-invalid="true"`, `class="disclosure rename" open`)
		oneH1(t, "rename "+n, w)
	}
	f.busy["p"] = true
	w := post("anders")
	wantStatus(t, w, 409)
	wantBody(t, w, "arbeitet gerade", `value="anders"`)
	f.busy["p"] = false

	if w := post("p"); w.Header().Get("Location") != "/projects/p" {
		t.Errorf("same name: %q", w.Header().Get("Location"))
	}
	w = post("anders")
	wantStatus(t, w, 303)
	if loc := w.Header().Get("Location"); loc != "/projects/anders?renamed=1" {
		t.Fatalf("Location %q", loc)
	}
	if _, ok := f.projects["anders"]; !ok {
		t.Fatal("not renamed")
	}
	wantBody(t, do(t, h, req{path: "/projects/anders?renamed=1"}), "Der Name ist jetzt „anders“.")
}

// --- portal-ux start ---

// taskBody builds the start form in DOM order: description, template, then the
// file parts (files alternates name and content).
func taskBody(t *testing.T, desc, taskTemplate string, files ...string) (io.Reader, string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("description", desc)
	if taskTemplate != "" {
		mw.WriteField("template", taskTemplate)
	}
	for i := 0; i+1 < len(files); i += 2 {
		fw, err := mw.CreateFormFile("files", files[i])
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(fw, files[i+1])
	}
	mw.Close()
	return &b, mw.FormDataContentType()
}

func TestCleanDescription(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		invalid           bool
	}{
		{"multiline", "\n  First line\r\n\r\n\tSecond line\n  ", "\n  First line\r\n\r\n\tSecond line\n  ", false},
		{"controls", "First\x00 line\x1b", "First line", false},
		{"whitespace", strings.Repeat("\n", 10), strings.Repeat("\n", 10), true},
		{"short", "  short text  ", "  short text  ", false},
		{"padded short", "   short   ", "   short   ", true},
		{"limit", strings.Repeat("ä", 1000), strings.Repeat("ä", 1000), false},
		{"over limit", strings.Repeat("ä", 1000) + "\n", strings.Repeat("ä", 1000) + "\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := cleanDescription(tc.input)
			if got != tc.want || (msg != "") != tc.invalid {
				t.Fatalf("cleanDescription = %q, %q", got, msg)
			}
		})
	}
}

func TestStartProject(t *testing.T) {
	h, f := setup(t)
	post := func(desc, taskTemplate string, files ...string) *httptest.ResponseRecorder {
		body, ct := taskBody(t, desc, taskTemplate, files...)
		return do(t, h, req{method: "POST", path: "/projects/start", body: body, ctype: ct})
	}
	nothing := func(what string) {
		t.Helper()
		if len(f.created) != 0 || len(f.sessions) != 0 || len(f.files) != 0 {
			t.Errorf("%s: created %v sessions %v files %d", what, f.created, f.sessions, len(f.files))
		}
	}

	// unfilled placeholder: refused before anything exists, text kept, files must be picked again
	w := post(findTemplate("research").in(langDE).Description, "research", "a.csv", "x")
	wantStatus(t, w, 422)
	wantBody(t, w, "Ersetze zuerst die Stellen in eckigen Klammern", `aria-invalid="true"`, "Recherchiere im Netz zu [Thema]",
		"noch einmal aus", `name="template" value="research"`)
	oneH1(t, "loslegen 422", w)
	nothing("placeholder")

	// Tabelle without a file: refused, message at the file field
	w = post("Werte die Tabelle im Eingangsordner aus: Besucher nach Monat.", "table")
	wantStatus(t, w, 422)
	wantBody(t, w, "braucht der Helfer eine Tabelle", `id="files-error"`, `href="#files"`)
	nothing("table")
	wantStatus(t, post("zu kurz", ""), 422)
	nothing("short")

	// text only: unique name from the suggestion, project, one chat with the text
	f.suggest = "Besucher Zahlen\nweil ..."
	f.addProject("besucher-zahlen", "")
	desc := "\n  Werte die Besucherzahlen 2025 aus.\r\n\r\n\tBitte nach Monat aufteilen.\n  "
	w = post(desc, "")
	wantStatus(t, w, 303)
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "https://anna.apps.example.org/") {
		t.Errorf("Location %q", loc)
	}
	if len(f.created) != 1 || (f.created[0].Name != "besucher-zahlen-2" || f.created[0].Description != desc) ||
		len(f.sessions) != 1 || f.sessions[0] != "besucher-zahlen-2|"+desc {
		t.Fatalf("created %v sessions %v", f.created, f.sessions)
	}

	// text and two files: both in inbox, named in the first prompt
	w = post("Werte die Tabelle im Eingangsordner aus: Besucher nach Monat.", "table", "a.csv", "1;2", "../b.xlsx", "y")
	wantStatus(t, w, 303)
	if f.created[1].Name != "besucher-zahlen-3" || string(f.files["besucher-zahlen-3/inbox/a.csv"]) != "1;2" || f.files["besucher-zahlen-3/inbox/b.xlsx"] == nil {
		t.Fatalf("created %v files %v", f.created, f.files)
	}
	if want := "besucher-zahlen-3|Werte die Tabelle im Eingangsordner aus: Besucher nach Monat.\n\nDateien im \x45ingang: a.csv, b.xlsx"; f.sessions[1] != want {
		t.Errorf("prompt %q", f.sessions[1])
	}

	// too large after the project exists: project page with the error at the upload field
	w = post("Lies die große Datei.", "", "gross.bin", strings.Repeat("x", 2<<20))
	wantStatus(t, w, 413)
	wantBody(t, w, "zu groß", "files-error")
	if len(f.sessions) != 2 {
		t.Errorf("chat started after a failed upload: %v", f.sessions)
	}

	// environment asleep: start it, park the chat, waiting page
	f.env.State = EnvStopped
	w = post("Schreibe einen Pressetext.", "")
	wantStatus(t, w, 303)
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/environment/starting?project=besucher-zahlen-5&t=") || len(f.sessions) != 2 || !slices.Contains(f.calls, "start") {
		t.Errorf("Location %q sessions %v calls %v", loc, f.sessions, f.calls)
	}
}

func TestStartPageUX(t *testing.T) {
	h, f := setup(t)
	// old links of the removed two-step form
	for path, want := range map[string]string{"/projects/new?template=table": "/?template=table", "/projects/new": "/", "/projects/new?template=x": "/"} {
		if w := do(t, h, req{path: path}); w.Code != 303 || w.Header().Get("Location") != want {
			t.Errorf("%s: %d %q", path, w.Code, w.Header().Get("Location"))
		}
	}
	for _, v := range taskTemplates {
		if v.ID != "table" && strings.Contains(v.Description+v.in(langDE).Description, "results") {
			t.Errorf("template %s sends answers to results instead of the chat", v.ID)
		}
	}
	if strings.Contains(tr(langDE, validName(""))+tr(langDE, validName(strings.Repeat("a", 51))), "Ordner") {
		t.Error("name messages say Ordner")
	}

	f.addProject("kaputt", "")
	f.projects["kaputt"].Status = StatusFailed
	f.projects["kaputt"].Recent = []Chat{{Title: "Neu", Link: "https://x/ok"}, {Title: "Kaputt", Status: StatusFailed, Link: "https://x/fail"}}
	f.addProject("laeuft", "")
	f.projects["laeuft"].Status = StatusWorking
	f.projects["laeuft"].Recent = []Chat{{Link: "https://x/l"}}
	f.addProject("leer", "")
	w := do(t, h, req{path: "/"})
	body := w.Body.String()
	wantBody(t, w, `<textarea class="textarea" id="description" name="description" rows="5" required minlength="10" maxlength="1000" data-submit-shortcut`,
		`aria-describedby="description-help description-shortcut"`, `id="description-shortcut"`,
		"Strg+Enter oder Cmd+Enter", ">Starten</button>")
	wantBody(t, w, `<h2 id="recent-h">Zuletzt bearbeitet</h2>`, `href="/projects"`, `href="/projects/kaputt"`,
		`class="filepick__input"`, `<label for="files" class="btn filepick__btn">Dateien dazulegen</label>`)
	wantBody(t, do(t, h, req{path: "/projects"}), "Der Helfer hat mit einem Fehler aufgehört.",
		`href="https://x/fail">Im Chat nachsehen: Kaputt`, `href="/projects/kaputt/delete" data-dialog`)

	if strings.Contains(body, "/projects/laeuft/delete") || strings.Contains(body, "Ordner") || strings.Contains(body, "Neuer Chat") {
		t.Error("trash while working, or old wording")
	}
	for st, want := range map[EnvState]string{EnvStopped: "Beim Öffnen eines Gesprächs startet dein Arbeitsbereich", EnvStarting: "Deine Gespräche sind in etwa 20 Sekunden bereit.", EnvError: `action="/environment/start"`} {
		f.env.State = st
		wantBody(t, do(t, h, req{path: "/"}), want)
	}

	// undo after trashing: the newest trash item of that name
	f.trash = []TrashItem{{ID: "leer--1", Name: "leer", DeletedAt: time.Now().Add(-time.Hour)}, {ID: "leer--2", Name: "leer", DeletedAt: time.Now()}, {ID: "x--1", Name: "x"}}
	w = do(t, h, req{path: "/?deleted=leer"})
	wantBody(t, w, "Das Projekt „leer“ liegt im Papierkorb.", `action="/trash/leer--2/restore"`, "Rückgängig", "Papierkorb (3)")
	oneH1(t, "undo", w)
}

// --- portal-ux rest ---

func TestUXRestStopConfirm(t *testing.T) {
	h, f := setup(t)
	f.env = EnvStatus{State: EnvRunning, Since: time.Now(), ActiveRuns: []string{"ses_1", "ses_2"}}
	wantBody(t, do(t, h, req{path: "/environment"}), `href="/environment/stop" data-dialog`)
	w := do(t, h, req{path: "/environment/stop"})
	wantStatus(t, w, http.StatusOK)
	wantBody(t, w, "dialog--inline", "an 2 Aufgaben", `name="confirmed" value="yes"`)
	oneH1(t, "anhalten", w)
	w = do(t, h, req{method: "POST", path: "/environment/stop", form: url.Values{}})
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantBody(t, w, "an 2 Aufgaben")
	if len(f.calls) != 0 {
		t.Fatalf("stop without confirmation: %v", f.calls)
	}
	w = do(t, h, req{method: "POST", path: "/environment/stop", form: url.Values{"confirmed": {"yes"}}})
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/environment?ok=stopped&aborted=1" {
		t.Errorf("Location %q", loc)
	}
	wantBody(t, do(t, h, req{path: "/environment?ok=stopped&aborted=1"}), "Laufende Aufgaben wurden abgebrochen.")

	// no runs: plain form, plain POST, old notice
	f.env.ActiveRuns, f.calls = nil, nil
	wantBody(t, do(t, h, req{path: "/environment"}), `<form method="post" action="/environment/stop">`)
	w = do(t, h, req{method: "POST", path: "/environment/stop", form: url.Values{}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/environment?ok=stopped" || len(f.calls) != 1 {
		t.Fatalf("plain stop: %d %q %v", w.Code, w.Header().Get("Location"), f.calls)
	}
	wantBody(t, do(t, h, req{path: "/environment?ok=stopped"}), "Sie startet beim nächsten Öffnen von selbst.")
}

func TestUXRestEpochIsNoTime(t *testing.T) {
	h, f := setup(t)
	f.addProject("neu", "")
	f.projects["neu"].LastActivity = time.UnixMilli(0)
	f.projects["neu"].Recent = []Chat{{ID: "ses_1", Updated: time.UnixMilli(0), Link: "https://x/session/ses_1"}}
	w := do(t, h, req{path: "/"})
	wantStatus(t, w, http.StatusOK)
	if b := w.Body.String(); strings.Contains(b, "1970") || strings.Contains(b, "lange nicht benutzt") {
		t.Error("epoch 0 shown as a date")
	}
	s := &Server{now: time.Now}
	if s.when(langDE, time.Unix(0, 0)) != "–" || dateOrDash(time.UnixMilli(0), "02.01.2006") != "–" {
		t.Error("epoch must read as no time")
	}
}

// TestUsageTodayLikeGate: "Heute" shows the gate's sum (Spent), so spend of a
// deleted account of the same uid counts and gets its own line.
func TestUsageTodayLikeGate(t *testing.T) {
	f := newFake()
	f.usage = []UsageRow{{Project: "p", CostEUR: 1}}
	spent := 4.0
	var asked time.Time
	day := time.Date(2026, 10, 17, 0, 0, 0, 0, time.Local)
	h, err := New(Config{Backend: f, Assets: os.DirFS("../../web"), Origin: "https://apps.example.org", Group: UserGroup, DayCapEUR: 5,
		DayStart: func(time.Time) time.Time { return day },
		Spent: func(_ context.Context, _ string, since time.Time) (float64, error) {
			asked = since
			return spent, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	wantBody(t, do(t, h, req{path: "/usage"}), "4,00 € von 5,00 € Tagesbudget", "Heute bereits verbraucht (vor der Neuanlage): 3,00&nbsp;€", `value="4"`)
	if !asked.Equal(day) {
		t.Fatalf("asked since %v", asked)
	}
	spent = 1
	if strings.Contains(do(t, h, req{path: "/usage"}).Body.String(), "vor der Neuanlage") {
		t.Error("no earlier spend, no line")
	}
	spent = 5
	wantBody(t, do(t, h, req{path: "/usage", header: map[string]string{"Accept-Language": "en"}}), "Already spent today (before the account was created again): ", "Daily budget reached")
}

func TestUXRestUsage(t *testing.T) {
	h, f := setup(t)
	f.usage = []UsageRow{{Project: "live", CostEUR: 1, CO2MinG: 6, CO2MaxG: 20, Chats: 2},
		{Project: "alt", Trashed: true, CostEUR: 0.5}, {CostEUR: 0.02}}
	w := do(t, h, req{path: "/usage"})
	wantBody(t, w, `<a href="/projects/live">live</a>`, `alt <span class="muted">(im Papierkorb)</span>`,
		"Ohne Projekt: Namensvorschläge", "Chats mit Verbrauch", "Das entspricht etwa 30 bis 90 Metern Autofahrt.")
	if strings.Contains(w.Body.String(), `href="/projects/alt"`) {
		t.Error("trashed project must not be linked")
	}
	f.usage = f.usage[:1]
	if strings.Contains(do(t, h, req{path: "/usage"}).Body.String(), "Ohne Projekt") {
		t.Error("no-project note without such a row")
	}
	wantBody(t, do(t, h, req{path: "/usage/method"}), "229,6&nbsp;g", "Umweltbundesamt")
	for _, c := range []struct {
		lo, hi float64
		want   string
	}{
		{0, 0, ""},
		{6, 20, "30 bis 90 Metern"},
		{230, 230, "1 Kilometer"},
		{100, 700, "0,4 bis 3 Kilometern"},
		{0.2, 0.2, "0,9 Metern"},
	} {
		if got := carRange(langDE, c.lo, c.hi); got != c.want {
			t.Errorf("carRange(%v, %v) = %q, want %q", c.lo, c.hi, got, c.want)
		}
	}
}

func TestUXRestRestoreAndFavicon(t *testing.T) {
	h, f := setup(t)
	f.trash = []TrashItem{{ID: "p--1", Name: "p"}}
	w := do(t, h, req{method: "POST", path: "/trash/p--1/restore", form: url.Values{}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/projects/p?restored=1" {
		t.Fatalf("restore: %d %q", w.Code, w.Header().Get("Location"))
	}
	w = do(t, h, req{path: "/favicon.ico", id: &Identity{}})
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/static/favicon.svg" {
		t.Fatalf("favicon.ico: %d %q", w.Code, w.Header().Get("Location"))
	}
	wantStatus(t, do(t, h, req{path: "/static/favicon.svg", id: &Identity{}}), http.StatusOK)
	wantBody(t, do(t, h, req{path: "/help"}), `<link rel="icon" href="/static/favicon.svg" type="image/svg+xml">`)
}

// CSP style-src 'self': no page may carry a style attribute or element, and
// htmx must not inject its indicator <style> (includeIndicatorStyles:false).
func TestUXRestNoInlineStyle(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "Beschreibung")
	f.trash = []TrashItem{{ID: "q--1", Name: "q"}}
	f.usage = []UsageRow{{Project: "p", CostEUR: 1, CO2MaxG: 3, Chats: 1}}
	for _, st := range []EnvState{EnvRunning, EnvStopped, EnvStarting, EnvError} {
		f.env = EnvStatus{State: st, ErrorID: "U-1", ActiveRuns: []string{"ses_1"}}
		for _, path := range []string{"/", "/?template=table", "/?deleted=q", "/projects/p", "/projects/p?uploaded=1", "/projects/p/delete", "/trash", "/trash/q--1/delete",
			"/environment", "/environment/stop", "/environment/starting", "/environment/reset", "/usage", "/usage/method",
			"/help", "/privacy", "/gibtsnicht"} {
			b := do(t, h, req{path: path}).Body.String()
			if strings.Contains(b, "style=") || strings.Contains(b, "<style") {
				t.Errorf("%s %s: inline style breaks CSP", st, path)
			}
			if !strings.Contains(b, `"includeIndicatorStyles":false`) {
				t.Errorf("%s %s: htmx-config lost includeIndicatorStyles:false", st, path)
			}
			// a trigger filter [..] is evaluated JS: allowEval:false logs htmx:evalDisallowedError
			if regexp.MustCompile(`hx-trigger="[^"]*\[`).MatchString(b) {
				t.Errorf("%s %s: hx-trigger filter needs eval", st, path)
			}
		}
	}
}

// --- portal-ux integration ---

// While the environment starts, the cards poll themselves (no trigger filter,
// allowEval is false); otherwise the section has no trigger and polling ends.
func TestUXIntegrationStartPolling(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "Beschreibung")
	f.env = EnvStatus{State: EnvStarting}
	wantBody(t, do(t, h, req{path: "/"}), `id="karten"`, `hx-get="/" hx-select="#karten" hx-target="#karten" hx-swap="outerHTML" hx-trigger="every 2s"`,
		"Deine Gespräche sind in etwa 20 Sekunden bereit.", `<a href="/">Neu laden</a>`)
	for _, st := range []EnvState{EnvRunning, EnvStopped, EnvError} {
		f.env = EnvStatus{State: st, ErrorID: "U-1"}
		b := do(t, h, req{path: "/"}).Body.String()
		if !strings.Contains(b, `id="karten"`) || strings.Contains(b, "hx-trigger") {
			t.Errorf("%s: cards must not poll", st)
		}
	}
	f.env = EnvStatus{State: EnvStopped}
	wantBody(t, do(t, h, req{path: "/"}), "Beim Öffnen eines Gesprächs startet dein Arbeitsbereich. Das dauert etwa 20 Sekunden.")
}

// Exactly one primary button on the project page, also after an upload; chats
// newest first; forms that start a run carry data-once.
func TestUXIntegrationProjectPage(t *testing.T) {
	h, f := setup(t)
	f.addProject("p", "Beschreibung")
	old, fresh := time.Now().Add(-time.Hour), time.Now()
	f.projects["p"].ChatList = []Chat{{ID: "ses_1", Title: "Alt", Updated: old, Link: "https://x/1"}, {ID: "ses_2", Title: "Neu", Updated: fresh, Link: "https://x/2"}}
	f.projects["p"].Chats = 2
	for _, path := range []string{"/projects/p", "/projects/p?uploaded=2"} {
		b := do(t, h, req{path: path}).Body.String()
		if n := strings.Count(b, "btn--primary"); n != 1 {
			t.Errorf("%s: %d primary buttons", path, n)
		}
		if head := b[:strings.Index(b, `id="results"`)]; !strings.Contains(head, "Weiterarbeiten: Neu") {
			t.Errorf("%s: head lacks the newest chat", path)
		}
		if i, j := strings.Index(b, ">Neu</th>"), strings.Index(b, ">Alt</th>"); i < 0 || j < i {
			t.Errorf("%s: chats not newest first", path)
		}
	}
	wantBody(t, do(t, h, req{path: "/projects/p?uploaded=2"}), `<a class="btn" href="https://x/2">Weiterarbeiten: Neu</a>`)
	f.projects["p"].ChatList, f.projects["p"].Chats = nil, 0
	b := do(t, h, req{path: "/projects/p?uploaded=1"}).Body.String()
	if strings.Count(b, "btn--primary") != 1 || strings.Count(b, "data-once") != 3 {
		t.Errorf("loslegen after upload: primary %d data-once %d", strings.Count(b, "btn--primary"), strings.Count(b, "data-once"))
	}
	wantBody(t, do(t, h, req{path: "/projects"}), `action="/projects/p/open" data-once`)
	wantBody(t, do(t, h, req{path: "/"}), "data-brief")
}

func TestRecentProjectsAndFullList(t *testing.T) {
	h, f := setup(t)
	now := time.Now()
	for i := range 7 {
		name := fmt.Sprintf("project-%d", i)
		f.addProject(name, "")
		f.projects[name].LastActivity = now.Add(time.Duration(i) * time.Hour)
	}
	recent := do(t, h, req{path: "/"})
	wantStatus(t, recent, 200)
	wantBody(t, recent, `href="/projects"`, "Zuletzt bearbeitet")
	var order []string
	for i := 6; i >= 2; i-- {
		order = append(order, fmt.Sprintf(`href="/projects/project-%d"`, i))
	}
	wantOrder(t, recent.Body.String(), order)
	if strings.Contains(recent.Body.String(), `href="/projects/project-1"`) {
		t.Fatal("start shows more than five recent projects")
	}
	all := do(t, h, req{path: "/projects"})
	wantStatus(t, all, 200)
	wantBody(t, all, "Alle Projekte", `href="/projects/project-0"`, `href="/projects/project-1"`)
	f.env.State = EnvStarting
	wantBody(t, do(t, h, req{path: "/projects"}), `hx-get="/projects"`)
}

func TestLogoutLinks(t *testing.T) {
	assets := os.DirFS("../../web")
	files, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		body, err := fs.ReadFile(assets, file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "/outpost.goauthentik.io/") {
			t.Errorf("hardcoded logout: %s", file)
		}
	}
	for _, logout := range []string{"", "/oauth2/sign_out"} {
		h, err := New(Config{Backend: newFake(), Assets: assets, Origin: "https://apps.example.org", Group: UserGroup, Consent: &fakeConsent{}, LogoutURL: logout})
		if err != nil {
			t.Fatal(err)
		}
		want := logout
		if want == "" {
			want = "/outpost.goauthentik.io/sign_out"
		}
		for _, lang := range []string{"en", "de"} {
			for _, path := range []string{"/", "/consent/declined", "/deleted", "/no-access-test"} {
				id := anna
				if path == "/no-access-test" {
					id = Identity{}
				}
				w := do(t, h, req{path: path, id: &id, header: map[string]string{"Accept-Language": lang}})
				status := http.StatusOK
				if path == "/no-access-test" {
					status = http.StatusForbidden
				}
				wantStatus(t, w, status)
				wantBody(t, w, `href="`+want+`"`)
				label := "Sign out"
				if lang == "de" {
					label = "Abmelden"
				}
				wantBody(t, w, label)
			}
		}
	}
}

func TestOAuth2ProxyAdminGroups(t *testing.T) {
	for _, tc := range []struct {
		name, required, admin   string
		groups                  []string
		homeStatus, adminStatus int
	}{
		{"member", "workspace-users", "workspace-admins", []string{"workspace-users"}, 200, 404},
		{"admin", "workspace-users", "workspace-admins", []string{"workspace-users", "workspace-admins"}, 200, 200},
		{"admin without access", "workspace-users", "workspace-admins", []string{"workspace-admins"}, 403, 403},
		{"admin disabled", "workspace-users", "", []string{"workspace-users", "workspace-admins"}, 200, 404},
		{"open access without admin", "", "", nil, 200, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := New(Config{Backend: newFake(), Assets: os.DirFS("../../web"), Group: tc.required, Admin: fakeAdmin{}, AdminGroup: tc.admin})
			if err != nil {
				t.Fatal(err)
			}
			id := anna
			id.Groups = tc.groups
			wantStatus(t, do(t, h, req{path: "/", id: &id}), tc.homeStatus)
			wantStatus(t, do(t, h, req{path: "/admin", id: &id}), tc.adminStatus)
		})
	}
}
