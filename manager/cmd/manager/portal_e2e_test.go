// SPDX-License-Identifier: MIT

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/catalog"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/consent"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/lifecycle/lctest"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/router"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

// stack is the whole edge with fakes only: real router, real portal adapter,
// real lifecycle.Service and store; the runtime runs the file scripts on the
// host against ws, OpenCode is lctest.OpenCode.
type stack struct {
	ctx     context.Context
	workers sync.WaitGroup

	edge http.Handler
	svc  *lifecycle.Service
	st   *store.Store
	rt   *lctest.Runtime
	oc   *lctest.OpenCode
	ws   string
	cat  *catalog.Service
	cfg  config.Config
	pdf  pdfFunc
}

type person struct{ uid, username, groups string }

var alice = person{"a7889ac42c1d44753644ec24acde57e8650e05b30d4b86f39dc0c96a98d25356", "alice", "authentik Admins|opencode-users"}

// newStack serves slot 1's OpenCode on <netPrefix>.1.10:4096: lifecycle
// derives that address itself, and all of 127.0.0.0/8 is loopback on Linux.
func newStack(t testing.TB, netPrefix string, configure ...func(*config.Config)) *stack {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir()) // the scripts compare realpath with the given path
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{RequiredGroup: "opencode-users", IdentityIssuer: "authentik",
		Domain: "apps.example.org", UsersRoot: "/srv/opencode/users", RuntimeImage: "img:1", AdminConfig: "opencode-config",
		ManagerContainer: "oc-manager", EgressContainer: "egress", UserNetPrefix: netPrefix,
		MasterKey: []byte("0123456789abcdef0123456789abcdef"), StartTimeout: 2 * time.Second,
		IdleTimeout: 30 * time.Minute, TabCap: 4 * time.Hour, DrainCap: 2 * time.Hour, MaxLifetime: 24 * time.Hour,
	}
	for _, fn := range configure {
		fn(&cfg)
	}
	oc := &lctest.OpenCode{}
	l, err := net.Listen("tcp", cfg.SlotAddr(1, config.HostUser).String()+":4096")
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: oc}}
	srv.Start()
	t.Cleanup(srv.Close)
	rt := lctest.NewRuntime()
	rt.Root = ws
	rt.OpenCode = oc
	log := slog.New(slog.DiscardHandler)
	svc := lifecycle.New(cfg, rt, st, log)
	svc.Suggest = func(context.Context, store.User, string) (string, error) { return "besucher-auswertung", nil }
	// stub converter: "Als PDF" shows for .md results without a Gotenberg
	pdf := func(_ context.Context, md []byte, _ string) ([]byte, error) {
		return append([]byte("%PDF-1.4\n% stub\n"), md...), nil
	}
	cat := catalogFor(t, rt, svc, st, log)
	ctx, cancel := context.WithCancel(context.Background())
	s := &stack{ctx: ctx, svc: svc, st: st, rt: rt, oc: oc, ws: ws, cat: cat, cfg: cfg, pdf: pdf}
	// Register after catalogFor creates its temp dirs: stop writers before removal.
	t.Cleanup(func() {
		cancel()
		s.workers.Wait()
		svc.Close()
		rt.Close()
	})
	s.buildEdge(t)
	return s
}

// buildEdge (re)creates portal and router from the package globals
// (portalTools, portalConns, portalOnboarding), as run() does.
func (s *stack) buildEdge(t testing.TB) {
	t.Helper()
	if err := s.st.CheckIdentityIssuer(context.Background(), s.cfg.IdentityIssuer); err != nil {
		t.Fatal(err)
	}
	portalIdentityGuard = s.st
	portalConsent, portalPrefs, portalSpent = consentStore{s.st}, s.st, s.st.SpentSince
	ph, err := newPortal(s.svc, s.cfg, s.pdf)
	if err != nil {
		t.Fatal(err)
	}
	s.edge = router.New(router.Router{
		Domain: s.cfg.Domain,
		Checker: identity.Checker{RequiredGroup: "opencode-users",
			Issuer: s.cfg.IdentityIssuer, SubjectHeader: s.cfg.IdentitySubjectHeader,
			Headers: s.cfg.IdentityHeaders, GroupsSeparator: s.cfg.IdentityGroupsSeparator,
			Trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("127.0.0.0/8")}},
		Users: s.st, LC: s.svc, Portal: ph, Consented: consented(s.st), Log: slog.New(slog.DiscardHandler),
	})
}

type call struct {
	method, url string
	form        url.Values
	body        io.Reader
	ctype       string
	as          *person  // nil: request from outside the trusted proxy, no headers
	noConsent   bool     // do not accept the prototype notice for as first
	hdr         []string // extra header name/value pairs
}

func (s *stack) do(t *testing.T, c call) *httptest.ResponseRecorder {
	t.Helper()
	if c.method == "" {
		c.method = http.MethodGet
	}
	if !strings.HasPrefix(c.url, "https://") {
		c.url = "https://apps.example.org" + c.url
	}
	if c.form != nil {
		c.body, c.ctype = strings.NewReader(c.form.Encode()), "application/x-www-form-urlencoded"
	}
	r := httptest.NewRequest(c.method, c.url, c.body)
	r.RemoteAddr = "203.0.113.9:40000"
	if c.ctype != "" {
		r.Header.Set("Content-Type", c.ctype)
	}
	if c.as != nil && !c.noConsent && identity.ValidUID(c.as.uid) && strings.Contains(c.as.groups, "opencode-users") {
		if ok, _ := s.st.HasConsent(r.Context(), c.as.uid, consent.Version()); !ok {
			if err := s.st.AcceptConsent(r.Context(), c.as.uid, consent.Version(), time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if c.as != nil {
		r.RemoteAddr = "10.0.0.2:40000"
		r.Header.Set("X-Authentik-Uid", c.as.uid)
		r.Header.Set("X-Authentik-Username", c.as.username)
		r.Header.Set("X-Authentik-Groups", c.as.groups)
	}
	for i := 0; i+1 < len(c.hdr); i += 2 {
		r.Header.Set(c.hdr[i], c.hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.edge.ServeHTTP(w, r)
	return w
}

func want(t *testing.T, w *httptest.ResponseRecorder, code int, subs ...string) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status %d, want %d; body:\n%s", w.Code, code, w.Body.String())
	}
	for _, s := range subs {
		if !strings.Contains(w.Body.String(), s) {
			t.Errorf("body lacks %q", s)
		}
	}
}

func wantLoc(t *testing.T, w *httptest.ResponseRecorder, prefix string) string {
	t.Helper()
	loc := w.Header().Get("Location")
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(loc, prefix) {
		t.Fatalf("status %d Location %q, want 303 %s...; body:\n%s", w.Code, loc, prefix, w.Body.String())
	}
	return loc
}

func (s *stack) waitRunning(t *testing.T) {
	t.Helper()
	for range 100 {
		if st, _ := s.svc.EnvStatus(context.Background(), alice.uid); st.State == lifecycle.EnvRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("environment did not start")
}

// loslegen posts the start form: beschreibung first, then the files.
func startProject(t *testing.T, s *stack, desc string, files ...string) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("description", desc)
	for _, f := range files {
		fw, _ := mw.CreateFormFile("files", f)
		io.WriteString(fw, "inhalt "+f)
	}
	mw.Close()
	return s.do(t, call{method: "POST", url: "/projects/start", body: &b, ctype: mw.FormDataContentType(), as: &alice})
}

// section returns the HTML from id="<id>" up to the next section.
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

// The portal-ux flows on the full stack: one-click start with files,
// Weiterarbeiten, Ergebnisse, trash undo, rename, start page without blocking.
func TestPortalE2EUX(t *testing.T) {
	s := newStack(t, "127.82")
	me := &alice
	ctx := context.Background()

	// the first visit creates the account and starts the environment in the background
	want(t, s.do(t, call{url: "/", as: me}), http.StatusOK, "Meine Projekte")
	s.waitRunning(t)

	// a placeholder gap is refused, nothing is created
	want(t, startProject(t, s, "Recherchiere zum Thema [Thema] und fasse zusammen.", "a.csv"), http.StatusUnprocessableEntity, "eckigen Klammern")
	if ps, _ := s.svc.ListProjects(ctx, alice.uid); len(ps) != 0 || len(s.oc.Sessions) != 0 {
		t.Fatalf("placeholder created %v sessions %v", ps, s.oc.Sessions)
	}

	// one click: project with the suggested name, files in inbox, chat with the description and the file list
	s.svc.Suggest = func(context.Context, store.User, string) (string, error) { return "auswertung", nil }
	desc := "Werte die Tabellen nach Monat aus und schreibe einen Bericht."
	link := wantLoc(t, startProject(t, s, desc, "a.csv", "b.xlsx"), "https://alice.apps.example.org/server/")
	if !strings.HasSuffix(link, "/session/ses_1") {
		t.Fatalf("link %q", link)
	}
	for _, f := range []string{"a.csv", "b.xlsx"} {
		if b, err := os.ReadFile(filepath.Join(s.ws, "auswertung/inbox", f)); err != nil || string(b) != "inhalt "+f {
			t.Fatalf("%s not in inbox: %q %v", f, b, err)
		}
	}
	if len(s.oc.Prompts) != 1 || !strings.Contains(s.oc.Prompts[0], desc) || !strings.Contains(s.oc.Prompts[0], "Dateien im \x45ingang: a.csv, b.xlsx") {
		t.Fatalf("prompts %v", s.oc.Prompts)
	}
	if d := s.oc.Sessions[0].Location.Directory; d != "/workspace/auswertung" {
		t.Fatalf("session dir %q", d)
	}

	// Weiterarbeiten: a second chat, newer and titled, is the next step on the project list and project page
	link2 := wantLoc(t, s.do(t, call{method: "POST", url: "/projects/auswertung/open", form: url.Values{}, as: me}), "https://alice.apps.example.org/server/")
	s.oc.Mu.Lock()
	s.oc.Sessions[0].Title, s.oc.Sessions[0].Time.Updated = "Erste Auswertung", float64(time.Now().Add(-time.Hour).UnixMilli())
	s.oc.Sessions[1].Title, s.oc.Sessions[1].Time.Updated = "Diagramme", float64(time.Now().UnixMilli())
	s.oc.Mu.Unlock()
	want(t, s.do(t, call{url: "/", as: me}), http.StatusOK, "Zuletzt bearbeitet", `href="/projects/auswertung"`, `href="/projects">Alle Projekte</a>`)
	want(t, s.do(t, call{url: "/projects", as: me}), http.StatusOK, `href="`+link2+`">Weiterarbeiten: Diagramme</a>`, `href="`+link+`">Weiterarbeiten: Erste Auswertung</a>`)
	b := s.do(t, call{url: "/projects/auswertung", as: me}).Body.String()
	if head := b[:strings.Index(b, `id="results"`)]; !strings.Contains(head, `href="`+link2+`">Weiterarbeiten: Diagramme</a>`) {
		t.Error("project head lacks Weiterarbeiten: Diagramme")
	}

	// Ergebnisse: files in results/ with download and PDF, exactly one results ZIP; none for an empty project
	os.WriteFile(filepath.Join(s.ws, "auswertung/results/tabelle.csv"), []byte("monat;summe\n"), 0o644)
	os.WriteFile(filepath.Join(s.ws, "auswertung/results/bericht.md"), []byte("# Bericht\n"), 0o644)
	b = s.do(t, call{url: "/projects/auswertung", as: me}).Body.String()
	res := section(b, "results")
	for _, sub := range []string{`href="/projects/auswertung/files/results/tabelle.csv"`, `href="/projects/auswertung/pdf?file=results%2Fbericht.md"`, ">Als PDF<"} {
		if !strings.Contains(res, sub) {
			t.Errorf("results lacks %q:\n%s", sub, res)
		}
	}
	if n := strings.Count(b, "zip?part=results"); n != 1 {
		t.Errorf("%d results ZIP links", n)
	}
	want(t, s.do(t, call{url: "/projects/auswertung/files/results/tabelle.csv", as: me}), http.StatusOK, "monat;summe")
	want(t, s.do(t, call{url: "/projects/auswertung/pdf?file=results%2Fbericht.md", as: me}), http.StatusOK, "%PDF-", "# Bericht")
	if err := s.svc.CreateProject(ctx, alice.uid, lifecycle.NewProject{Name: "leer"}); err != nil {
		t.Fatal(err)
	}
	if b := s.do(t, call{url: "/projects/leer", as: me}).Body.String(); strings.Contains(b, "zip?part=") {
		t.Error("empty project offers a ZIP")
	}

	// trash with undo from the start page
	wantLoc(t, s.do(t, call{method: "POST", url: "/projects/leer/delete", form: url.Values{}, as: me}), "/?deleted=leer")
	trash, _ := s.svc.ListTrash(ctx, alice.uid)
	if len(trash) != 1 {
		t.Fatalf("trash %v", trash)
	}
	want(t, s.do(t, call{url: "/?deleted=leer", as: me}), http.StatusOK, `action="/trash/`+trash[0].ID+`/restore"`, "Rückgängig")
	wantLoc(t, s.do(t, call{method: "POST", url: "/trash/" + trash[0].ID + "/restore", form: url.Values{}, as: me}), "/projects/leer?restored=1")
	want(t, s.do(t, call{url: "/projects/leer?restored=1", as: me}), http.StatusOK, "ist wiederhergestellt")
	if fi, err := os.Stat(filepath.Join(s.ws, "leer")); err != nil || !fi.IsDir() {
		t.Fatalf("restored folder missing: %v", err)
	}

	// rename: folder and sessions move, the page lives under the new name
	wantLoc(t, s.do(t, call{method: "POST", url: "/projects/auswertung/rename", form: url.Values{"new_name": {"monatsbericht"}}, as: me}),
		"/projects/monatsbericht?renamed=1")
	if _, err := os.Stat(filepath.Join(s.ws, "auswertung")); !os.IsNotExist(err) {
		t.Error("old folder still there")
	}
	if _, err := os.Stat(filepath.Join(s.ws, "monatsbericht/inbox/a.csv")); err != nil {
		t.Errorf("renamed folder: %v", err)
	}
	s.oc.Mu.Lock()
	for _, se := range s.oc.Sessions {
		if se.Location.Directory != "/workspace/monatsbericht" {
			t.Errorf("session %s still in %s", se.ID, se.Location.Directory)
		}
	}
	s.oc.Mu.Unlock()
	want(t, s.do(t, call{url: "/projects/monatsbericht?renamed=1", as: me}), http.StatusOK, "<h1>monatsbericht</h1>", "Weiterarbeiten: Diagramme")
	want(t, s.do(t, call{url: "/projects/auswertung", as: me}), http.StatusNotFound)

	// the start page never waits for a start: stopped, it answers at once with
	// the projects; once running, the project list shows the chats
	s.do(t, call{method: "POST", url: "/environment/stop", form: url.Values{}, as: me})
	want(t, s.do(t, call{url: "/", as: me}), http.StatusOK, "monatsbericht", `id="karten"`)
	s.waitRunning(t)
	want(t, s.do(t, call{url: "/", as: me}), http.StatusOK, `href="/projects/monatsbericht"`, `href="/projects">Alle Projekte</a>`)
	want(t, s.do(t, call{url: "/projects", as: me}), http.StatusOK, "Weiterarbeiten: Diagramme")
}

func TestPortalE2E(t *testing.T) {
	s := newStack(t, "127.81")
	me := &alice
	ctx := context.Background()

	// identity: headers from outside the proxy or without uid never reach the portal
	want(t, s.do(t, call{url: "/"}), http.StatusForbidden)
	want(t, s.do(t, call{url: "/", as: &person{"x", "alice", "opencode-users"}}), http.StatusForbidden)
	stranger := &person{strings.Repeat("b", 64), "gast", "andere"}
	want(t, s.do(t, call{url: "/", as: stranger}), http.StatusForbidden, "Kein Zugang")
	if _, err := s.st.UserByUID(ctx, stranger.uid); err == nil {
		t.Error("no account for people outside the group")
	}
	want(t, s.do(t, call{url: "/", as: me}), http.StatusOK, "Meine Projekte")

	// a project without chat (the portal always opens one, so create it in the core): folder layout, project page
	desc := "Werte die Besucherzahlen 2025 aus und schreibe einen kurzen Bericht."
	if err := s.svc.CreateProject(ctx, alice.uid, lifecycle.NewProject{Name: "besucher", Description: desc}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"inbox", "results", "work", ".git"} {
		if fi, err := os.Stat(filepath.Join(s.ws, "besucher", d)); err != nil || !fi.IsDir() {
			t.Fatalf("%s missing: %v", d, err)
		}
	}
	want(t, s.do(t, call{url: "/projects/besucher", as: me}), http.StatusOK, "besucher", "Besucherzahlen 2025")
	want(t, s.do(t, call{url: "/", as: me}), http.StatusOK, `href="/projects/besucher"`, `href="/projects">Alle Projekte</a>`)
	want(t, s.do(t, call{url: "/projects", as: me}), http.StatusOK, `href="/projects/besucher"`, `href="/projects/besucher/delete"`, `href="/projects/besucher#results"`)
	want(t, s.do(t, call{url: "/projects/besucher", as: me}), http.StatusOK, `href="/projects/besucher/delete" data-dialog`, "In den Papierkorb")

	// "Loslegen" on the start page with the environment running: name from the suggestion, chat with the prompt, deep link
	s.svc.Suggest = func(context.Context, store.User, string) (string, error) { return "presse", nil }
	var tb bytes.Buffer
	tw := multipart.NewWriter(&tb)
	tw.WriteField("description", "Schreibe einen Pressetext.")
	tw.Close()
	link := wantLoc(t, s.do(t, call{method: "POST", url: "/projects/start", body: &tb, ctype: tw.FormDataContentType(), as: me}),
		"https://alice.apps.example.org/server/")
	if !strings.HasSuffix(link, "/session/ses_1") || len(s.oc.Prompts) != 1 || !strings.Contains(s.oc.Prompts[0], "Pressetext") {
		t.Fatalf("link %q prompts %v", link, s.oc.Prompts)
	}
	if s.oc.Sessions[0].Location.Directory != "/workspace/presse" {
		t.Errorf("session dir %q", s.oc.Sessions[0].Location.Directory)
	}
	// the deep link host proxies to the person's OpenCode
	want(t, s.do(t, call{url: "https://alice.apps.example.org/api/info", as: me}), http.StatusOK, "2.2.0")
	want(t, s.do(t, call{url: "/projects/presse", as: me}), http.StatusOK, `href="`+link+`"`)

	// the project list shows the chats with a direct link, "Loslegen" for a project without chat
	want(t, s.do(t, call{url: "/projects", as: me}), http.StatusOK, `href="`+link+`"`, `action="/projects/besucher/open" data-once`, `name="brief" value="1"`)

	// "Weiterarbeiten" continues the newest chat; without a chat it shows the project page
	wantLoc(t, s.do(t, call{url: "/projects/presse/resume", as: me}), link)
	wantLoc(t, s.do(t, call{url: "/projects/besucher/resume", as: me}), "/projects/besucher")

	// "Anlegen und Dateien hochladen" left besucher without chat: "Loslegen" sends the stored description once
	want(t, s.do(t, call{url: "/projects/besucher", as: me}), http.StatusOK, `name="brief" value="1"`)
	wantLoc(t, s.do(t, call{method: "POST", url: "/projects/besucher/open", form: url.Values{"brief": {"1"}}, as: me}), "https://alice.apps.example.org/server/")
	if len(s.oc.Prompts) != 2 || !strings.Contains(s.oc.Prompts[1], "Besucherzahlen 2025") {
		t.Fatalf("loslegen prompts %v", s.oc.Prompts)
	}
	if strings.Contains(s.do(t, call{url: "/projects/besucher", as: me}).Body.String(), `name="brief"`) {
		t.Error("Loslegen still offered after the first chat")
	}
	wantLoc(t, s.do(t, call{method: "POST", url: "/projects/besucher/open", form: url.Values{"brief": {"1"}}, as: me}), "https://alice.apps.example.org/server/")
	if len(s.oc.Prompts) != 2 {
		t.Fatalf("second Loslegen sent a prompt: %v", s.oc.Prompts)
	}

	// upload into inbox, tree, download
	var mb bytes.Buffer
	mw := multipart.NewWriter(&mb)
	fw, _ := mw.CreateFormFile("files", "zahlen 2025.csv")
	io.WriteString(fw, "monat;besucher\njan;1200\n")
	mw.Close()
	wantLoc(t, s.do(t, call{method: "POST", url: "/projects/besucher/upload", body: &mb, ctype: mw.FormDataContentType(), as: me}),
		"/projects/besucher?uploaded=1")
	if b, err := os.ReadFile(filepath.Join(s.ws, "besucher/inbox/zahlen 2025.csv")); err != nil || !strings.HasPrefix(string(b), "monat;") {
		t.Fatalf("upload not in workspace: %v", err)
	}
	os.WriteFile(filepath.Join(s.ws, "besucher/results/bericht.md"), []byte("# Bericht\n"), 0o644)
	want(t, s.do(t, call{url: "/projects/besucher", as: me}), http.StatusOK,
		"zahlen 2025.csv", `href="/projects/besucher/files/inbox/zahlen%202025.csv"`, "bericht.md")
	w := s.do(t, call{url: "/projects/besucher/files/inbox/zahlen%202025.csv", as: me})
	want(t, w, http.StatusOK, "jan;1200")
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("Content-Disposition %q", cd)
	}
	want(t, s.do(t, call{url: "/projects/besucher/files/.project.json", as: me}), http.StatusNotFound)

	// ZIP: regular files only, no dot files
	for part, files := range map[string]string{"all": "besucher/inbox/zahlen 2025.csv|besucher/results/bericht.md", "results": "besucher/bericht.md"} {
		w = s.do(t, call{url: "/projects/besucher/zip?part=" + part, as: me})
		want(t, w, http.StatusOK)
		zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range zr.File {
			names = append(names, f.Name)
		}
		slices.Sort(names)
		if strings.Join(names, "|") != files {
			t.Errorf("zip %s: %v", part, names)
		}
	}

	// trash: move, list, restore, move again, purge only with confirmation
	wantLoc(t, s.do(t, call{method: "POST", url: "/projects/besucher/delete", form: url.Values{}, as: me}), "/")
	if _, err := os.Stat(filepath.Join(s.ws, "besucher")); !os.IsNotExist(err) {
		t.Fatal("project folder still there")
	}
	want(t, s.do(t, call{url: "/trash", as: me}), http.StatusOK, "besucher", "Wiederherstellen")
	trash, _ := s.svc.ListTrash(ctx, alice.uid)
	if len(trash) != 1 {
		t.Fatalf("trash %v", trash)
	}
	id := trash[0].ID
	wantLoc(t, s.do(t, call{method: "POST", url: "/trash/" + id + "/restore", form: url.Values{}, as: me}), "/projects/besucher")
	want(t, s.do(t, call{url: "/projects/besucher", as: me}), http.StatusOK, "zahlen 2025.csv")
	want(t, s.do(t, call{url: "/trash", as: me}), http.StatusOK, "Der Papierkorb ist leer")

	wantLoc(t, s.do(t, call{method: "POST", url: "/projects/besucher/delete", form: url.Values{}, as: me}), "/")
	trash, _ = s.svc.ListTrash(ctx, alice.uid)
	id = trash[0].ID
	want(t, s.do(t, call{url: "/trash/" + id + "/delete", as: me}), http.StatusOK, "besucher")
	want(t, s.do(t, call{method: "POST", url: "/trash/" + id + "/delete", form: url.Values{}, as: me}), http.StatusUnprocessableEntity)
	if _, err := os.Stat(filepath.Join(s.ws, ".trash", id)); err != nil {
		t.Fatal("purge without confirmation removed the folder")
	}
	wantLoc(t, s.do(t, call{method: "POST", url: "/trash/" + id + "/delete", form: url.Values{"confirmed": {"yes"}}, as: me}), "/trash")
	if _, err := os.Stat(filepath.Join(s.ws, ".trash", id)); !os.IsNotExist(err) {
		t.Fatal("purged folder still there")
	}
	want(t, s.do(t, call{url: "/trash", as: me}), http.StatusOK, "Der Papierkorb ist leer")

	// Umgebung: stop (asks while a task runs), start; the new OpenCode has no runs
	want(t, s.do(t, call{url: "/environment", as: me}), http.StatusOK, "läuft")
	s.oc.Mu.Lock()
	s.oc.Active = []string{"ses_1"}
	s.oc.Mu.Unlock()
	want(t, s.do(t, call{method: "POST", url: "/environment/stop", form: url.Values{}, as: me}), http.StatusUnprocessableEntity, "an einer Aufgabe")
	wantLoc(t, s.do(t, call{method: "POST", url: "/environment/stop", form: url.Values{"confirmed": {"yes"}}, as: me}), "/environment?ok=stopped&aborted=1")
	if len(s.rt.Ctrs) != 0 {
		t.Fatal("container still there after stop")
	}
	want(t, s.do(t, call{url: "/environment", as: me}), http.StatusOK, "schläft")
	wantLoc(t, s.do(t, call{method: "POST", url: "/environment/start", form: url.Values{}, as: me}), "/environment/starting")
	s.waitRunning(t)
	want(t, s.do(t, call{url: "/environment", as: me}), http.StatusOK, "läuft")
	if w := s.do(t, call{url: "/", as: me}); strings.Contains(w.Body.String(), ">Helfer arbeitet<") {
		t.Fatal("start page still shows a run that the stop aborted")
	}

	// "Neuer Chat" while asleep: start page, then the token post opens the chat
	s.do(t, call{method: "POST", url: "/environment/stop", form: url.Values{}, as: me})
	wait := wantLoc(t, s.do(t, call{method: "POST", url: "/projects/presse/open", form: url.Values{}, as: me}), "/environment/starting?project=presse&t=")
	s.waitRunning(t)
	want(t, s.do(t, call{url: wait, as: me}), http.StatusOK, `hx-post="/projects/presse/open"`)
	tok := wait[strings.LastIndex(wait, "=")+1:]
	wantLoc(t, s.do(t, call{method: "POST", url: "/projects/presse/open", form: url.Values{"t": {tok}}, as: me}),
		"https://alice.apps.example.org/server/")

	// Usage: usage per project; events without project and of purged projects share one row
	press, _ := s.st.ProjectByFolder(ctx, alice.uid, "presse")
	for i, e := range []struct {
		project, session string
		eur              float64
	}{{press.ID, "ses_1", 1.00}, {press.ID, "ses_2", 0.50}, {"", "", 0.25}, {"zzzz-geloescht", "ses_9", 0.75}} {
		now := time.Now()
		if err := s.st.InsertUsage(ctx, store.UsageEvent{EventID: string(rune('a' + i)), Start: now, End: now, UID: alice.uid,
			ProjectID: e.project, SessionID: e.session, Capability: "llm", Status: 200, CostEUR: &e.eur}); err != nil {
			t.Fatal(err)
		}
	}
	want(t, s.do(t, call{url: "/usage", as: me}), http.StatusOK, "presse", "1,50 €", "1,00 €", "2,50 €")
}
