// SPDX-License-Identifier: MIT

package lifecycle

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestProjectFiles(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.s.CreateSession(ctx, uid1, "bericht", ""); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("want ErrNotRunning, got %v", err)
	}
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "bericht", Description: "Zahlen 2025 auswerten"}); err != nil {
		t.Fatal(err)
	}
	p, _ := e.st.ProjectByFolder(ctx, uid1, "bericht")
	dir := filepath.Join(e.s.ws, "bericht")
	for _, d := range []string{"inbox", "results", "work", ".git"} {
		if fi, err := os.Stat(filepath.Join(dir, d)); err != nil || !fi.IsDir() {
			t.Fatalf("%s missing", d)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".git/opencode")); strings.TrimSpace(string(b)) != p.ID {
		t.Fatal(".git/opencode must hold the project id")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".project/brief.md")); !strings.Contains(string(b), "Zahlen 2025") {
		t.Fatal("brief.md")
	}
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "bericht"}); !errors.Is(err, ErrExists) {
		t.Fatal("duplicate must be ErrExists", err)
	}
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "../x"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}

	// upload with collision naming, then a failing reader leaves nothing behind
	for _, want := range []string{"daten.csv", "daten (2).csv"} {
		got, err := e.s.Upload(ctx, uid1, "bericht", "daten.csv", strings.NewReader("a;b\n"))
		if err != nil || got != want {
			t.Fatalf("upload: %q %v", got, err)
		}
	}
	if _, err := e.s.Upload(ctx, uid1, "bericht", "kaputt.txt", iotest.ErrReader(errors.New("boom"))); err == nil {
		t.Fatal("failing reader must fail the upload")
	}
	if _, err := os.Stat(filepath.Join(dir, "inbox/kaputt.txt")); !os.IsNotExist(err) {
		t.Fatal("partial upload must be removed")
	}

	files, _, err := e.s.Tree(ctx, uid1, "bericht")
	if i := slices.IndexFunc(files, func(f File) bool { return f.Path == "inbox/daten (2).csv" }); err != nil || i < 0 || files[i].Size != 4 {
		t.Fatalf("%+v %v", files, err)
	}

	// symlinks are never followed
	os.WriteFile(filepath.Join(e.s.ws, "geheim.txt"), []byte("x"), 0o600)
	os.Symlink(filepath.Join(e.s.ws, "geheim.txt"), filepath.Join(dir, "results/link.txt"))
	os.Symlink(filepath.Join(dir, "inbox"), filepath.Join(dir, "results/linkdir"))
	os.WriteFile(filepath.Join(dir, "results/bericht.md"), []byte("# Bericht"), 0o600)
	if _, _, err := e.s.OpenFile(ctx, uid1, "bericht", "results/link.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatal("symlink must be refused", err)
	}
	if _, _, err := e.s.OpenFile(ctx, uid1, "bericht", "results/linkdir/daten.csv"); !errors.Is(err, ErrNotFound) {
		t.Fatal("symlinked dir must be refused", err)
	}
	rc, fi, err := e.s.OpenFile(ctx, uid1, "bericht", "results/bericht.md")
	if err != nil || fi.Size != 9 {
		t.Fatal(fi, err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "# Bericht" {
		t.Fatal(string(b))
	}
	if _, _, err := e.s.OpenFile(ctx, uid1, "bericht", ".project/brief.md"); !errors.Is(err, ErrInvalid) {
		t.Fatal("dot path must be refused", err)
	}
	if _, _, err := e.s.OpenFile(ctx, uid1, "bericht", "results/../../x"); !errors.Is(err, ErrInvalid) {
		t.Fatal("dotdot must be refused", err)
	}
	// WriteNew never overwrites and never writes through a symlinked dir
	for _, want := range []string{"results/bericht.pdf", "results/bericht_v2.pdf"} {
		if got, err := e.s.WriteNew(ctx, uid1, "bericht", "results/bericht.pdf", strings.NewReader("%PDF")); err != nil || got != want {
			t.Fatalf("write new: %q %v", got, err)
		}
	}
	if _, err := e.s.WriteNew(ctx, uid1, "bericht", "results/linkdir/x.pdf", strings.NewReader("%PDF")); !errors.Is(err, ErrNotFound) {
		t.Fatal("symlinked dir must be refused", err)
	}
	if _, err := e.s.WriteNew(ctx, uid1, "bericht", ".project/x.pdf", strings.NewReader("%PDF")); !errors.Is(err, ErrInvalid) {
		t.Fatal("dot path must be refused", err)
	}
	if files, _, _ := e.s.Tree(ctx, uid1, "bericht"); slices.ContainsFunc(files, func(f File) bool { return strings.Contains(f.Path, "link") }) {
		t.Fatalf("tree must skip links: %+v", files)
	}

	// a hard link is exported as a regular file under each name (tar TypeLink otherwise)
	os.Link(filepath.Join(dir, "results/bericht.md"), filepath.Join(dir, "results/zweitname.md"))
	var zbuf bytes.Buffer
	if err := e.s.WriteZip(ctx, uid1, "bericht", "all", &zbuf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(zbuf.Bytes()), int64(zbuf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if !slices.Contains(names, "bericht/results/bericht.md") || !slices.Contains(names, "bericht/results/zweitname.md") || slices.ContainsFunc(names, func(n string) bool {
		return strings.Contains(n, "/.") || strings.Contains(n, "link")
	}) {
		t.Fatalf("zip entries %v", names)
	}

	// chat creation waits for the folder load and the stored prompt, then links
	defer func(p time.Duration) { settlePoll = p }(settlePoll)
	settlePoll = time.Millisecond
	defer func(g time.Duration) { mcpToolsGrace = g }(mcpToolsGrace)
	mcpToolsGrace = 0
	e.oc.LoadingFor, e.oc.Queued, e.oc.MCPPendingFor = 2, 2, 3
	link, err := e.s.CreateSession(ctx, uid1, "bericht", "Bitte auswerten")
	origin := "https://alice.apps.example.org"
	if err != nil || link != origin+"/server/"+base64.RawURLEncoding.EncodeToString([]byte(origin))+"/session/ses_1" {
		t.Fatal(link, err)
	}
	if len(e.oc.Prompts) != 1 {
		t.Fatal("prompt not sent")
	}
	if e.oc.LoadingFor != 0 || e.oc.Queued != 0 {
		t.Fatal("redirect before the folder loaded or the prompt was stored")
	}
	if e.oc.MCPPendingFor != 0 || e.oc.MCPGets < 4 {
		t.Fatalf("prompt sent before the workspace mcp connected (pending left %d, gets %d)", e.oc.MCPPendingFor, e.oc.MCPGets)
	}
	// a folder that never settles still gets its link once the wait runs out
	defer func(d time.Duration) { settleTimeout = d }(settleTimeout)
	settleTimeout = 20 * time.Millisecond
	e.oc.LoadingFor = 1 << 30
	if link, err := e.s.CreateSession(ctx, uid1, "bericht", ""); err != nil || !strings.HasSuffix(link, "/session/ses_2") {
		t.Fatal(link, err)
	}
	e.oc.Mu.Lock() // a model poll the settle wait gave up on may still be served
	e.oc.LoadingFor = 0
	e.oc.Sessions = e.oc.Sessions[:1]
	e.oc.Active = []string{"ses_1"}
	e.oc.Mu.Unlock()
	if err := e.s.MoveToTrash(ctx, uid1, "bericht"); !errors.Is(err, ErrBusy) {
		t.Fatal("busy project must not be trashed", err)
	}
	e.oc.Active = nil
	if err := e.s.MoveToTrash(ctx, uid1, "bericht"); err != nil {
		t.Fatal(err)
	}
	trash, _ := e.st.Trash(ctx, uid1)
	if len(trash) != 1 {
		t.Fatal("trash row")
	}
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "bericht"}); !errors.Is(err, ErrExists) {
		t.Fatal("trashed name stays taken", err)
	}
	if err := e.s.Restore(ctx, uid1, trash[0].ID, "bericht-alt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.s.ws, "bericht-alt/results/bericht.md")); err != nil {
		t.Fatal("restore", err)
	}
	if err := e.s.MoveToTrash(ctx, uid1, "bericht-alt"); err != nil {
		t.Fatal(err)
	}
	trash, _ = e.st.Trash(ctx, uid1)
	if err := e.s.Purge(ctx, uid1, trash[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.s.trashDir(trash[0].ID)); !os.IsNotExist(err) {
		t.Fatal("purge must delete the folder")
	}
}

func TestTree(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "baum"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.s.ws, "baum")
	deep := filepath.Join(dir, "work", strings.Repeat("d/", treeMaxDepth))
	os.MkdirAll(deep, 0o700)
	for _, f := range []string{"notiz ä.md", "work/tab\tund\nzeile.txt", ".env", "work/.cache/x", ".opencode/y"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o700)
		os.WriteFile(filepath.Join(dir, f), []byte("abc"), 0o600)
	}
	os.Symlink("/etc", filepath.Join(dir, "etc"))
	files, truncated, err := e.s.Tree(ctx, uid1, "baum")
	if err != nil || !truncated {
		t.Fatal(truncated, err)
	}
	got := map[string]File{}
	for _, f := range files {
		got[f.Path] = f
	}
	if f := got["notiz ä.md"]; f.Size != 3 || f.Dir || f.Name != "notiz ä.md" || f.Modified.IsZero() {
		t.Errorf("%+v", f)
	}
	if f := got["work/tab\tund\nzeile.txt"]; f.Name != "tab\tund\nzeile.txt" {
		t.Errorf("tab/newline name: %+v", f)
	}
	if !got["inbox"].Dir || !got["work/d"].Dir {
		t.Error("dirs missing")
	}
	for p := range got {
		if hidden(p) || strings.HasPrefix(p, "etc") || strings.Count(p, "/") >= treeMaxDepth {
			t.Errorf("must be hidden: %q", p)
		}
	}
	if _, _, err := e.s.Tree(ctx, uid1, "../x"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestParseTree(t *testing.T) {
	files, truncated := parseTree("d\t4096\t1.5\tresults\x00f\t7\t1760000000.25\tresults/a\tb.txt\x00f\t1\t1\t.x\x00\x00garbage\x00")
	if truncated || len(files) != 2 || files[0].Size != 0 || !files[0].Dir || files[1].Name != "a\tb.txt" || files[1].Size != 7 ||
		files[1].Modified.UnixMilli() != 1760000000250 {
		t.Fatalf("%+v", files)
	}
	files, truncated = parseTree(strings.Repeat("f\t1\t1\tx\x00", treeMaxEntries+1))
	if !truncated || len(files) != treeMaxEntries {
		t.Fatal(len(files), truncated)
	}
}
