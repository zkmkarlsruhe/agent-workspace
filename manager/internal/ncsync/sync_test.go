// SPDX-License-Identifier: MIT

package ncsync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/identity"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/ocapi"
	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/store"
)

var uid = strings.Repeat("a", 64)

type env struct {
	s       *Service
	dav     *fakeDAV
	files   *fakeFiles
	st      *store.Store
	clock   *clock
	consent bool
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.EnsureUser(ctx, identity.Identity{UID: uid, Username: "z"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProject(ctx, store.Project{ID: "p1", UID: uid, Folder: "bericht", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	dav := newFakeDAV()
	srv := httptest.NewServer(dav)
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		dav.mu.Lock()
		defer dav.mu.Unlock()
		if len(dav.bad) > 0 {
			t.Errorf("destructive or out-of-root requests: %q", dav.bad)
		}
	})
	base, _ := url.Parse(srv.URL)
	c := &clock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	files := newFakeFiles(c.now)
	s := &Service{DAV: NewDAV(base), Store: st, Files: files, Log: slog.New(slog.DiscardHandler), now: c.now,
		Creds: func(context.Context, string) (Creds, error) { return Creds{"z", "z", "pw"}, nil }}
	e := env{s, dav, files, st, c, true}
	s.Consented = func(context.Context, string) (bool, error) { return e.consent, nil }
	return &e
}

func TestKickDebounce(t *testing.T) {
	e := setup(t)
	e.s.init()
	t0 := e.clock.now()
	e.s.Kick(uid)
	e.clock.add(time.Second)
	e.s.Kick(uid)
	if got := e.s.due[uid]; !got.Equal(t0.Add(3 * time.Second)) {
		t.Fatalf("a new trigger moves the pass: %v", got.Sub(t0))
	}
	for range 20 {
		e.clock.add(time.Second)
		e.s.Kick(uid)
	}
	if got := e.s.due[uid]; !got.Equal(t0.Add(10 * time.Second)) {
		t.Fatalf("MaxWait caps the debounce: %v", got.Sub(t0))
	}
}

func TestSyncPushConflictPullRename(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.s.Link(ctx, uid, "bericht", false); err != nil {
		t.Fatal(err)
	}
	if !e.dav.dirs["Agent Workspace/bericht"] || !strings.Contains(e.files.meta["bericht/nextcloud.md"], "`Agent Workspace/bericht/`") {
		t.Fatalf("link: dirs %v meta %q", e.dav.dirs, e.files.meta)
	}
	old := e.clock.now().Add(-time.Minute)
	e.files.write("bericht", "results/folien.pptx", "v1", old)
	e.files.write("bericht", "results/bilder/a.png", "png", old)
	e.files.write("bericht", "work/notiz.txt", "x", old)
	e.files.write("bericht", "inbox/quelle.pdf", "x", old)
	e.files.write("bericht", "results/halb.docx", "...", e.clock.now()) // still being written

	if again := e.s.Pass(ctx, uid); !again {
		t.Fatal("a fresh file must ask for another pass")
	}
	if got, _ := e.dav.get("Agent Workspace/bericht/folien.pptx"); got != "v1" {
		t.Fatalf("push: %q", got)
	}
	if _, ok := e.dav.get("Agent Workspace/bericht/bilder/a.png"); !ok {
		t.Fatal("subfolder of results")
	}
	for _, p := range []string{"Agent Workspace/bericht/halb.docx", "Agent Workspace/bericht/work/notiz.txt", "Agent Workspace/bericht/inbox/quelle.pdf"} {
		if _, ok := e.dav.get(p); ok {
			t.Fatalf("%s must not be pushed", p)
		}
	}
	e.clock.add(3 * time.Second)
	if again := e.s.Pass(ctx, uid); again {
		t.Fatal("nothing fresh left")
	}
	if _, ok := e.dav.get("Agent Workspace/bericht/halb.docx"); !ok {
		t.Fatal("file pushed once it settled")
	}
	reads := e.files.reads
	e.s.Pass(ctx, uid)
	if e.files.reads != reads {
		t.Fatal("unchanged files must not be read again")
	}

	// the helper updates in place: same name in Nextcloud
	e.files.write("bericht", "results/folien.pptx", "v2", e.clock.now().Add(-5*time.Second))
	e.s.Pass(ctx, uid)
	if got, _ := e.dav.get("Agent Workspace/bericht/folien.pptx"); got != "v2" {
		t.Fatalf("update in place: %q", got)
	}

	// the person edits in Nextcloud, then the helper writes again: both kept
	e.dav.set("Agent Workspace/bericht/folien.pptx", "v2-person")
	e.files.write("bericht", "results/folien.pptx", "v3", e.clock.now().Add(-4*time.Second))
	e.s.Pass(ctx, uid)
	if got, _ := e.dav.get("Agent Workspace/bericht/folien.pptx"); got != "v2-person" {
		t.Fatalf("the person's edit was overwritten: %q", got)
	}
	if got, _ := e.dav.get("Agent Workspace/bericht/folien (Helfer).pptx"); got != "v3" {
		t.Fatalf("helper copy: %q", got)
	}
	st, _ := e.s.Status(ctx, uid, "bericht")
	if !st.Linked || !strings.Contains(st.Note, "folien (Helfer).pptx") || !strings.Contains(st.Note, "wurde in Nextcloud geändert") || st.LastSync.IsZero() || !strings.Contains(st.URL, "dir=%2FAgent+Workspace%2Fbericht") {
		t.Fatalf("status %+v", st)
	}

	// pull: the person's version comes back in place, a new file from Nextcloud too
	e.dav.set("Agent Workspace/bericht/von-mir.txt", "neu")
	e.clock.add(time.Minute)
	res, err := e.s.Pull(ctx, uid, "bericht", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Updated, []string{"results/folien.pptx"}) || !slices.Equal(res.New, []string{"results/von-mir.txt"}) || len(res.Conflicts) != 0 {
		t.Fatalf("pull %+v", res)
	}
	if got, _ := e.files.read("bericht", "results/folien.pptx"); got != "v2-person" {
		t.Fatalf("pulled content %q", got)
	}
	e.clock.add(time.Minute)
	calls := len(e.dav.calls)
	e.s.Pass(ctx, uid)
	for _, m := range e.dav.calls[calls:] {
		if m == "PUT" {
			t.Fatal("pulled files must not be pushed back")
		}
	}
	// the pulled version is the person's (D58): the helper's next write goes to a copy
	e.files.write("bericht", "results/folien.pptx", "v4", e.clock.now().Add(-5*time.Second))
	e.s.Pass(ctx, uid)
	if got, _ := e.dav.get("Agent Workspace/bericht/folien.pptx"); got != "v2-person" {
		t.Fatalf("after pull the person's version was overwritten: %q", got)
	}
	if got, _ := e.dav.get("Agent Workspace/bericht/folien (Helfer 2).pptx"); got != "v4" {
		t.Fatalf("after pull: copy %q", got)
	}

	// changed on both sides: the project keeps the helper's, inbox/ gets the person's
	e.dav.set("Agent Workspace/bericht/folien.pptx", "v4-person")
	e.files.write("bericht", "results/folien.pptx", "v5", e.clock.now())
	res, err = e.s.Pull(ctx, uid, "bericht", "results/folien.pptx")
	if err != nil || !slices.Equal(res.Conflicts, []string{"inbox/folien (Nextcloud).pptx"}) {
		t.Fatalf("both changed: %+v %v", res, err)
	}
	if got, _ := e.files.read("bericht", "results/folien.pptx"); got != "v5" {
		t.Fatalf("helper's version must stay: %q", got)
	}

	// rename of the project: the Nextcloud folder follows; the note is in the portal language
	if err := e.st.SetLang(ctx, uid, "en"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.RenameProject(ctx, "p1", "bericht-2026"); err != nil {
		t.Fatal(err)
	}
	e.files.rename("bericht", "bericht-2026")
	e.clock.add(time.Minute)
	e.s.Pass(ctx, uid)
	if _, ok := e.dav.get("Agent Workspace/bericht-2026/von-mir.txt"); !ok || e.dav.dirs["Agent Workspace/bericht"] {
		t.Fatalf("rename not followed: %v", e.dav.dirs)
	}
	// the conflict of the pull stands: the person's version stays the original, the
	// helper's goes to a fresh copy; the earlier copy (maybe edited by the person) stays
	if got, _ := e.dav.get("Agent Workspace/bericht-2026/folien.pptx"); got != "v4-person" {
		t.Fatalf("push after a pull conflict overwrote the person's version: %q", got)
	}
	if got, _ := e.dav.get("Agent Workspace/bericht-2026/folien (Helfer 3).pptx"); got != "v5" {
		t.Fatalf("fresh helper copy: %q", got)
	}
	if got, _ := e.dav.get("Agent Workspace/bericht-2026/folien (Helfer).pptx"); got != "v3" {
		t.Fatalf("existing copy overwritten: %q", got)
	}
	if st, _ := e.s.Status(ctx, uid, "bericht-2026"); !strings.Contains(st.Note, "“folien.pptx” was changed in Nextcloud") {
		t.Fatalf("English note: %q", st.Note)
	}
	// fetching brings the person's version in; it stays hers, the helper's next write is a copy
	e.clock.add(time.Minute)
	if res, err := e.s.Pull(ctx, uid, "bericht-2026", "results/folien.pptx"); err != nil || !slices.Equal(res.Updated, []string{"results/folien.pptx"}) {
		t.Fatalf("resolve: %+v %v", res, err)
	}
	e.files.write("bericht-2026", "results/folien.pptx", "v6", e.clock.now().Add(-5*time.Second))
	e.s.Pass(ctx, uid)
	if got, _ := e.dav.get("Agent Workspace/bericht-2026/folien.pptx"); got != "v4-person" {
		t.Fatalf("after fetching: %q", got)
	}
	if got, _ := e.dav.get("Agent Workspace/bericht-2026/folien (Helfer 4).pptx"); got != "v6" {
		t.Fatalf("after fetching: copy %q", got)
	}
	if !strings.Contains(e.files.meta["bericht-2026/nextcloud.md"], "Agent Workspace/bericht-2026/") {
		t.Fatal("meta after rename")
	}

	if err := e.s.Unlink(ctx, uid, "bericht-2026"); err != nil {
		t.Fatal(err)
	}
	if e.files.meta["bericht-2026/nextcloud.md"] != "" {
		t.Fatal("meta must go with the link")
	}
	if _, err := e.s.Pull(ctx, uid, "bericht-2026", ""); !errors.Is(err, ErrNotLinked) {
		t.Fatal(err)
	}
}

func TestUploadAndCollections(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	old := e.clock.now().Add(-time.Minute)
	e.files.write("bericht", "results/bericht.pdf", "pdf", old)
	// the helper's own file is replaced while unchanged (live view in OnlyOffice);
	// once the person changed it, the helper's next version goes to _v2 (D58)
	for i, step := range []struct{ content, personEdit, want, wantOrig string }{
		{"pdf", "", "Agent Workspace/bericht/bericht.pdf", "pdf"},
		{"pdf2", "", "Agent Workspace/bericht/bericht.pdf", "pdf2"},
		{"pdf3", "von der Person", "Agent Workspace/bericht/bericht_v2.pdf", "von der Person"},
	} {
		if step.personEdit != "" {
			e.dav.set("Agent Workspace/bericht/bericht.pdf", step.personEdit)
		}
		e.files.write("bericht", "results/bericht.pdf", step.content, old.Add(time.Duration(i)*time.Second))
		got, err := e.s.Upload(ctx, uid, "bericht", "results/bericht.pdf", "")
		if err != nil || got != step.want {
			t.Fatalf("unlinked ablegen %d: %q %v", i, got, err)
		}
		if orig, _ := e.dav.get("Agent Workspace/bericht/bericht.pdf"); orig != step.wantOrig {
			t.Fatalf("unlinked ablegen %d: original is %q, want %q", i, orig, step.wantOrig)
		}
	}
	e.dav.dirs["Team"], e.dav.dirs["Team/Presse"] = true, true
	e.dav.set("Team/Presse/bericht.pdf", "fremd")
	for _, dir := range []string{"Team/Presse", "/Team/Presse", "Agent Workspace/../Team/Presse", "Agent Workspace/%2e%2e/Team",
		"Agent Workspace/%2E%2E", "Agent Workspacex", "Agent Workspace/.versteckt", "Agent Workspace/a\x00b", `Agent Workspace\..\Team`} {
		if got, err := e.s.Upload(ctx, uid, "bericht", "results/bericht.pdf", dir); !errors.Is(err, ErrOutside) {
			t.Errorf("ablegen into %q: %q %v", dir, got, err)
		}
	}
	if got, _ := e.dav.get("Team/Presse/bericht.pdf"); got != "fremd" {
		t.Fatal("a file outside the transfer folder changed")
	}
	if got, err := e.s.Upload(ctx, uid, "bericht", "results/bericht.pdf", "/Agent Workspace/Presse/"); err != nil || got != "Agent Workspace/Presse/bericht.pdf" {
		t.Fatalf("subfolder of the transfer folder: %q %v", got, err)
	}
	// linked: same name, updated in place
	if err := e.s.Link(ctx, uid, "bericht", false); err != nil {
		t.Fatal(err)
	}
	e.files.write("bericht", "results/neu.md", "a", e.clock.now())
	if got, err := e.s.Upload(ctx, uid, "bericht", "results/neu.md", ""); err != nil || got != "Agent Workspace/bericht/neu.md" {
		t.Fatalf("linked ablegen of a fresh file: %q %v", got, err)
	}
	if _, err := e.s.Upload(ctx, uid, "bericht", "results/fehlt.md", ""); err == nil {
		t.Fatal("missing file")
	}

	if err := e.s.AddCollection(ctx, uid, "bericht", "Team/Presse"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AddCollection(ctx, uid, "bericht", "Team/gibt-es-nicht"); !errors.Is(err, ErrNotFolder) {
		t.Fatal(err)
	}
	if err := e.s.AddCollection(ctx, uid, "bericht", "Team/Presse/bericht.pdf"); !errors.Is(err, ErrNotFolder) {
		t.Fatal("a file is no collection", err)
	}
	if err := e.s.AddCollection(ctx, uid, "bericht", "../x"); !errors.Is(err, ErrNotFolder) {
		t.Fatal(err)
	}
	st, _ := e.s.Status(ctx, uid, "bericht")
	if len(st.Collections) != 1 || st.Collections[0].Path != "Team/Presse" {
		t.Fatalf("%+v", st.Collections)
	}
	if m := e.files.meta["bericht/nextcloud.md"]; !strings.Contains(m, "- `Team/Presse`") || !strings.Contains(m, "Dorthin schreibst du nie") {
		t.Fatalf("meta %q", m)
	}
	hits, err := e.s.Search(ctx, uid, "Team", "BERICHT", 100)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search %v %v", hits, err)
	}
	e.s.RemoveCollection(ctx, uid, "bericht", "Team/Presse")
	if strings.Contains(e.files.meta["bericht/nextcloud.md"], "Team/Presse") {
		t.Fatal("collection removed")
	}
}

func TestNotConnected(t *testing.T) {
	e := setup(t)
	e.s.Creds = func(context.Context, string) (Creds, error) { return Creds{}, errors.New("no") }
	if err := e.s.Link(context.Background(), uid, "bericht", false); !errors.Is(err, ErrNotConnected) {
		t.Fatal(err)
	}
	if len(e.dav.calls) != 0 {
		t.Fatal("no upstream call without a login")
	}
}

func TestExpired(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.s.Link(ctx, uid, "bericht", false); err != nil {
		t.Fatal(err)
	}
	var expired bool
	e.s.Expired = func(context.Context, string) { expired = true }
	e.s.Creds = func(context.Context, string) (Creds, error) { return Creds{"z", "z", "revoked"}, nil }
	e.files.write("bericht", "results/a.txt", "a", e.clock.now().Add(-time.Minute))
	e.s.Pass(ctx, uid)
	if !expired {
		t.Fatal("401 must mark the login expired")
	}
}

func TestEventStreamKicks(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.s.Link(ctx, uid, "bericht", false); err != nil {
		t.Fatal(err)
	}
	sent := make(chan struct{})
	oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `data: {"type":"server.connected","data":{}}`+"\n\n")
		w.(http.Flusher).Flush()
		<-sent
		io.WriteString(w, `data: {"type":"session.tool.success","location":{"directory":"/workspace/bericht"},"data":{"native":{"provider":"replay"}}}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer oc.Close()
	e.files.running = []string{uid}
	e.files.client = &ocapi.Client{BaseURL: oc.URL, Password: "pw"}
	e.s.reconcile(ctx) // the poll part kicks once
	e.s.mu.Lock()
	delete(e.s.due, uid)
	delete(e.s.first, uid)
	watched := e.s.watch[uid] != nil
	e.s.mu.Unlock()
	if !watched {
		t.Fatal("no watcher for a running linked person")
	}
	close(sent)
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.s.mu.Lock()
		_, ok := e.s.due[uid]
		e.s.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tool event did not kick a pass")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.files.running = nil
	e.s.reconcile(ctx)
	if e.s.watch[uid] != nil {
		t.Fatal("watcher must stop with the environment")
	}
}

// Without consent to the current notice the background sync neither
// watches nor pushes; work already due is dropped.
func TestSyncNeedsConsent(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.s.Link(ctx, uid, "bericht", false); err != nil {
		t.Fatal(err)
	}
	e.files.write("bericht", "results/a.txt", "a", e.clock.now().Add(-time.Minute))
	e.s.Kick(uid)
	e.consent = false
	e.files.running = []string{uid}
	e.s.reconcile(ctx)
	if e.s.watch[uid] != nil {
		t.Fatal("watcher without consent")
	}
	calls := len(e.dav.calls)
	if e.s.Pass(ctx, uid) {
		t.Fatal("no retry without consent")
	}
	if len(e.dav.calls) != calls {
		t.Fatalf("Nextcloud calls without consent: %v", e.dav.calls[calls:])
	}
	e.consent = true
	e.s.Pass(ctx, uid)
	if _, ok := e.dav.get("Agent Workspace/bericht/a.txt"); !ok {
		t.Fatal("push after consent")
	}
}

// D58: only a version the manager itself wrote may be overwritten. A pulled
// or pre-existing file of the person always gets a "(Helfer)" copy, also via
// nextcloud_upload, and a PUT without ETag gives no overwrite right either.
func TestPersonFileNeverOverwritten(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.s.Link(ctx, uid, "bericht", false); err != nil {
		t.Fatal(err)
	}
	e.dav.set("Agent Workspace/bericht/von-mir.txt", "person")
	if res, err := e.s.Pull(ctx, uid, "bericht", ""); err != nil || !slices.Equal(res.New, []string{"results/von-mir.txt"}) {
		t.Fatalf("pull %+v %v", res, err)
	}
	e.clock.add(time.Minute)
	e.files.write("bericht", "results/von-mir.txt", "", e.clock.now().Add(-5*time.Second))
	got, err := e.s.Upload(ctx, uid, "bericht", "results/von-mir.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := e.dav.get("Agent Workspace/bericht/von-mir.txt"); v != "person" {
		t.Fatalf("pulled person file overwritten by ablegen: %q", v)
	}
	if _, ok := e.dav.get("Agent Workspace/bericht/von-mir (Helfer).txt"); !ok {
		t.Fatalf("no helper copy, got %q", got)
	}
	// the background pass the same
	e.files.write("bericht", "results/von-mir.txt", "x", e.clock.now().Add(-5*time.Second))
	e.s.Pass(ctx, uid)
	if v, _ := e.dav.get("Agent Workspace/bericht/von-mir.txt"); v != "person" {
		t.Fatalf("pulled person file overwritten by the pass: %q", v)
	}

	// no ETag on the manager's PUT: the next write must not adopt one by PROPFIND
	e.dav.noETag, e.dav.raceEdit = true, "person-edit"
	e.files.write("bericht", "results/neu.txt", "v1", e.clock.now().Add(-5*time.Second))
	e.s.Pass(ctx, uid)
	e.dav.noETag = false
	e.files.write("bericht", "results/neu.txt", "v2", e.clock.now().Add(-4*time.Second))
	e.s.Pass(ctx, uid)
	if v, _ := e.dav.get("Agent Workspace/bericht/neu.txt"); v != "person-edit" {
		t.Fatalf("file without a manager ETag overwritten: %q", v)
	}
}
