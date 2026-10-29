// SPDX-License-Identifier: MIT

package lifecycle

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncFileOps(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.s.CreateProject(ctx, uid1, NewProject{Name: "bericht", Description: "x"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.s.ws, "bericht")
	os.MkdirAll(filepath.Join(dir, "results/bilder"), 0o755)
	os.WriteFile(filepath.Join(dir, "results/folien.pptx"), []byte("v1"), 0o644)
	os.WriteFile(filepath.Join(dir, "results/bilder/a.png"), []byte("png"), 0o644)
	os.WriteFile(filepath.Join(dir, "results/.~lock.folien.pptx#"), []byte("x"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(dir, "results/link.txt"))

	files, err := e.s.ScanFiles(ctx, uid1, "bericht", "results")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.Path)
	}
	if strings.Join(got, ",") != "results/bilder/a.png,results/folien.pptx" && strings.Join(got, ",") != "results/folien.pptx,results/bilder/a.png" {
		t.Fatalf("scan: %v", got)
	}
	if _, err := e.s.ScanFiles(ctx, uid1, "bericht", "../x"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}

	rc, err := e.s.ReadFile(ctx, uid1, "bericht", "results/folien.pptx")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "v1" {
		t.Fatalf("read %q", b)
	}
	if _, err := e.s.ReadFile(ctx, uid1, "bericht", "results/link.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("symlink read: %v", err)
	}

	if err := e.s.ReplaceFile(ctx, uid1, "bericht", "results/folien.pptx", strings.NewReader("v2")); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ReplaceFile(ctx, uid1, "bericht", "results/neu/von mir.txt", strings.NewReader("neu")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "results/folien.pptx")); string(b) != "v2" {
		t.Fatalf("replace %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "results/neu/von mir.txt")); string(b) != "neu" {
		t.Fatalf("replace into new folder %q", b)
	}
	if err := e.s.ReplaceFile(ctx, uid1, "bericht", "results/link.txt", strings.NewReader("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replace onto a symlink: %v", err)
	}
	os.Symlink("/tmp", filepath.Join(dir, "results/raus"))
	if err := e.s.ReplaceFile(ctx, uid1, "bericht", "results/raus/x.txt", strings.NewReader("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replace through a symlinked folder: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "results/.*.nc-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}

	if err := e.s.WriteMeta(ctx, uid1, "bericht", "nextcloud.md", "# hallo\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".project/nextcloud.md")); string(b) != "# hallo\n" {
		t.Fatalf("meta %q", b)
	}
	if err := e.s.WriteMeta(ctx, uid1, "bericht", "nextcloud.md", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".project/nextcloud.md")); !os.IsNotExist(err) {
		t.Fatal("meta must be removed")
	}
	if err := e.s.WriteMeta(ctx, uid1, "bericht", "../x", "a"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
