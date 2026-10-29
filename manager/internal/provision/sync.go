// SPDX-License-Identifier: MIT

package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// SyncConfig copies every top-level directory of from (the config baked into
// the image, e.g. /opt/workspace/{etc-opencode,catalog}) to the same name below to
// (/srv/opencode/base). It reports the names it replaced.
func SyncConfig(from, to string) ([]string, error) {
	ents, err := os.ReadDir(from)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		c, err := SyncDir(filepath.Join(from, e.Name()), filepath.Join(to, e.Name()))
		if err != nil {
			return changed, fmt.Errorf("sync %s: %w", e.Name(), err)
		}
		if c {
			changed = append(changed, e.Name())
		}
	}
	return changed, nil
}

// SyncDir makes dst a copy of src owned root:root, files 0644, dirs 0755.
// Unchanged content, owner and modes: nothing is written. A first install is
// built in dst.new and renamed into place. An update works inside dst like
// rsync --delay-updates: every changed file is staged next to its target, then
// all are renamed in one pass, then entries missing from src are removed.
// dst itself is never replaced: user containers and the manager bind-mount it
// as a directory, and a renamed-away tree would stay in their mounts.
func SyncDir(src, dst string) (bool, error) {
	want, err := treeHash(src, true)
	if err != nil {
		return false, err
	}
	have, err := treeHash(dst, false)
	if errors.Is(err, fs.ErrNotExist) {
		tmp := dst + ".new"
		if err := os.RemoveAll(tmp); err != nil {
			return false, err
		}
		if err := copyTree(src, tmp); err != nil {
			return false, err
		}
		return true, os.Rename(tmp, dst)
	}
	if err != nil {
		return false, err
	}
	if have == want {
		return false, nil
	}
	return true, updateTree(src, dst)
}

const stagePrefix = ".oc-sync-"

func updateTree(src, dst string) error {
	keep := map[string]bool{}
	var staged [][2]string // tmp, final
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		keep[out] = true
		fi, err := os.Lstat(out)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if d.IsDir() {
			if fi != nil && !fi.IsDir() {
				if err := os.Remove(out); err != nil {
					return err
				}
				fi = nil
			}
			if fi == nil {
				if err := os.Mkdir(out, 0o755); err != nil {
					return err
				}
			}
			return chownMode(out, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only files and directories are synced", p)
		}
		if fi != nil && fi.IsDir() {
			if err := os.RemoveAll(out); err != nil { // a directory where a file goes: cannot be staged
				return err
			}
		} else if fi != nil && sameFile(p, out, fi) {
			return nil
		}
		tmp := filepath.Join(filepath.Dir(out), stagePrefix+filepath.Base(out))
		if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := copyFile(p, tmp); err != nil {
			return err
		}
		staged = append(staged, [2]string{tmp, out})
		return nil
	})
	if err != nil {
		return err
	}
	for _, s := range staged {
		if err := os.Rename(s[0], s[1]); err != nil {
			return err
		}
	}
	// entries not in src, including stage files of an aborted run
	return filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil || keep[p] {
			return err
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
}

// sameFile: dst already is a root:root 0644 regular file with src's content.
func sameFile(src, dst string, fi fs.FileInfo) bool {
	st := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 || st.Uid != 0 || st.Gid != 0 {
		return false
	}
	a, err1 := fileHash(src)
	b, err2 := fileHash(dst)
	return err1 == nil && err2 == nil && string(a) == string(b)
}

// treeHash covers relative path, type, mode, owner and content of every entry.
// normalized hashes src as it will be written (root:root, 0644/0755).
func treeHash(root string, normalized bool) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		mode, uid, gid := fi.Mode()&(fs.ModeType|fs.ModePerm), st.Uid, st.Gid
		if normalized {
			if !fi.Mode().IsRegular() && !fi.IsDir() {
				return fmt.Errorf("%s: only files and directories are synced", p)
			}
			mode, uid, gid = fi.Mode()&fs.ModeType|0o644, 0, 0
			if fi.IsDir() {
				mode |= 0o111
			}
		}
		fmt.Fprintf(h, "%q %o %d %d ", rel, mode, uid, gid)
		if fi.Mode().IsRegular() {
			sum, err := fileHash(p)
			if err != nil {
				return err
			}
			h.Write(sum)
		}
		h.Write([]byte{'\n'})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileHash(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			if err := os.Mkdir(out, 0o755); err != nil {
				return err
			}
			return chownMode(out, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only files and directories are synced", p)
		}
		return copyFile(p, out)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return chownMode(dst, 0o644)
}

// chownMode sets root:root and the mode explicitly: the umask must not narrow it.
func chownMode(p string, mode os.FileMode) error {
	if err := os.Lchown(p, 0, 0); err != nil {
		return err
	}
	return os.Chmod(p, mode)
}
