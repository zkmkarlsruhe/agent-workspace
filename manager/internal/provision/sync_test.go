// SPDX-License-Identifier: MIT

package provision

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		want := os.FileMode(0o644)
		if fi.IsDir() {
			want = os.ModeDir | 0o755
		}
		if fi.Mode() != want || st.Uid != 0 || st.Gid != 0 {
			t.Errorf("%s: mode %v owner %d:%d", p, fi.Mode(), st.Uid, st.Gid)
		}
		if !fi.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			got[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestSyncConfig(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for chown (run in the golang container)")
	}
	from, to := t.TempDir(), t.TempDir()
	v1 := map[string]string{"AGENTS.md": "eins", "skills/pdf/SKILL.md": "pdf"}
	writeTree(t, filepath.Join(from, "etc-opencode"), v1)
	writeTree(t, filepath.Join(from, "catalog"), map[string]string{"mcp.json": "{}"})
	os.WriteFile(filepath.Join(from, "stray-file"), nil, 0o644) // not a directory: ignored

	changed, err := SyncConfig(from, to)
	if err != nil || len(changed) != 2 {
		t.Fatalf("first sync: %v %v", changed, err)
	}
	if got := readTree(t, filepath.Join(to, "etc-opencode")); len(got) != 2 || got["skills/pdf/SKILL.md"] != "pdf" {
		t.Fatalf("installed %v", got)
	}
	if _, err := os.Stat(filepath.Join(to, "stray-file")); err == nil {
		t.Fatal("file at the top level synced")
	}

	// unchanged: nothing written, the inode stays
	var before syscall.Stat_t
	syscall.Stat(filepath.Join(to, "etc-opencode"), &before)
	if changed, err := SyncConfig(from, to); err != nil || len(changed) != 0 {
		t.Fatalf("second sync: %v %v", changed, err)
	}
	var after syscall.Stat_t
	syscall.Stat(filepath.Join(to, "etc-opencode"), &after)
	if before.Ino != after.Ino {
		t.Fatal("unchanged tree was rewritten")
	}

	// changed content, a removed file, a dir that becomes a file and back, a stage
	// file of an aborted run, a symlink and a stray file planted on the host
	et := filepath.Join(to, "etc-opencode")
	os.Remove(filepath.Join(from, "etc-opencode", "skills/pdf/SKILL.md"))
	os.Remove(filepath.Join(from, "etc-opencode", "skills/pdf"))
	writeTree(t, filepath.Join(from, "etc-opencode"), map[string]string{"AGENTS.md": "zwei", "skills/pdf": "jetzt Datei", "neu/x.md": "x"})
	writeTree(t, et, map[string]string{".oc-sync-AGENTS.md": "halb", "fremd.md": "f"})
	os.Symlink("/etc/shadow", filepath.Join(et, "neu"))
	keepFd, err := os.Open(filepath.Join(et, "AGENTS.md")) // a reader holding the old file
	if err != nil {
		t.Fatal(err)
	}
	defer keepFd.Close()
	changed, err = SyncConfig(from, to)
	if err != nil || len(changed) != 1 || changed[0] != "etc-opencode" {
		t.Fatalf("third sync: %v %v", changed, err)
	}
	got := readTree(t, et)
	if len(got) != 3 || got["AGENTS.md"] != "zwei" || got["skills/pdf"] != "jetzt Datei" || got["neu/x.md"] != "x" {
		t.Fatalf("after change %v", got)
	}
	var now syscall.Stat_t
	syscall.Stat(et, &now)
	if now.Ino != before.Ino {
		t.Fatal("directory replaced: bind mounts of it would keep the old tree")
	}
	if b, _ := io.ReadAll(keepFd); string(b) != "eins" {
		t.Fatalf("old file changed in place: %q", b) // renamed over, never rewritten
	}
	if changed, err := SyncConfig(from, to); err != nil || len(changed) != 0 {
		t.Fatalf("after update not converged: %v %v", changed, err)
	}
	// back: file becomes a directory again
	os.Remove(filepath.Join(from, "etc-opencode", "skills/pdf"))
	writeTree(t, filepath.Join(from, "etc-opencode"), map[string]string{"skills/pdf/SKILL.md": "pdf2"})
	if _, err := SyncConfig(from, to); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, et); got["skills/pdf/SKILL.md"] != "pdf2" {
		t.Fatalf("file to dir: %v", got)
	}

	// a wrong mode on the host copy (hand edit) counts as a change
	os.Chmod(filepath.Join(to, "etc-opencode", "AGENTS.md"), 0o600)
	if changed, err := SyncConfig(from, to); err != nil || len(changed) != 1 {
		t.Fatalf("mode drift: %v %v", changed, err)
	}
	readTree(t, filepath.Join(to, "etc-opencode"))
}

func TestSyncRefusesSymlinks(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for chown (run in the golang container)")
	}
	from, to := t.TempDir(), t.TempDir()
	writeTree(t, filepath.Join(from, "catalog"), map[string]string{"a": "a"})
	os.Symlink("/etc/shadow", filepath.Join(from, "catalog", "link"))
	if _, err := SyncConfig(from, to); err == nil {
		t.Fatal("symlink in the source synced")
	}
	if _, err := os.Stat(filepath.Join(to, "catalog")); !os.IsNotExist(err) {
		t.Fatalf("partial install: %v", err)
	}
}
