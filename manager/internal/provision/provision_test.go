// SPDX-License-Identifier: MIT

package provision

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fakeQuota keeps project ids by inode, like xfs keeps them on the inode.
type fakeQuota struct {
	mu      sync.Mutex
	ids     map[uint64]uint32
	limits  []uint32
	cleared []uint32
}

func ino(fd int) uint64 {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		panic(err)
	}
	return st.Ino
}

func (f *fakeQuota) ProjID(fd int) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ids[ino(fd)], nil
}

func (f *fakeQuota) SetProjID(fd int, id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids[ino(fd)] = id
	return nil
}

func (f *fakeQuota) SetLimit(id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = append(f.limits, id)
	return nil
}

func (f *fakeQuota) ClearLimit(id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, id)
	return nil
}

// Usage reports 1 MB per project id that has a limit, like a dquot; others are unknown.
func (f *fakeQuota) Usage(id uint32) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.limits {
		if l == id {
			return 1e6, 20 << 30, nil
		}
	}
	return 0, 0, nil
}

func (f *fakeQuota) idOf(t *testing.T, path string) uint32 {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return f.ids[st.Ino]
}

const (
	uidA = "a7889ac42c1d44753644ec24acde57e8650e05b30d4b86f39dc0c96a98d25356"
	uidB = "b7889ac42c1d44753644ec24acde57e8650e05b30d4b86f39dc0c96a98d25356"
)

func setup(t *testing.T) (*Provisioner, *fakeQuota) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root for chown (run in the golang container)")
	}
	q := &fakeQuota{ids: map[uint64]uint32{}}
	return &Provisioner{Base: t.TempDir(), Q: q}, q
}

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		uid  string
		slot int
		ok   bool
	}{
		{uidA, 1, true},
		{uidA, 254, true},
		{uidA, 0, false},
		{uidA, 255, false},
		{uidA, -1, false},
		{strings.ToUpper(uidA), 1, false},
		{uidA[:63], 1, false},
		{uidA + "0", 1, false},
		{"../" + uidA[3:], 1, false},
		{uidA[:63] + "/", 1, false},
		{"", 1, false},
	} {
		err := Validate(c.uid, c.slot)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrInvalid)) {
			t.Errorf("Validate(%q, %d) = %v", c.uid, c.slot, err)
		}
	}
}

// The kernel copies sizeof(struct if_dqblk) = 72 bytes into the buffer.
func TestDqblkSize(t *testing.T) {
	if n := unsafe.Sizeof(ifDqblk{}); n != 72 {
		t.Fatalf("ifDqblk is %d bytes", n)
	}
}

func TestEnsureIdempotent(t *testing.T) {
	p, q := setup(t)
	for range 2 {
		if err := p.Ensure(uidA, 3); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range append([]string{""}, Subdirs...) {
		path := filepath.Join(p.Base, uidA, d)
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if !fi.IsDir() || fi.Mode().Perm() != 0o750 || st.Uid != Owner || st.Gid != Owner {
			t.Fatalf("%s: mode %v uid %d gid %d", path, fi.Mode(), st.Uid, st.Gid)
		}
		if id := q.idOf(t, path); id != 10003 {
			t.Fatalf("%s: project %d", path, id)
		}
	}
	if len(q.limits) != 2 || q.limits[0] != 10003 {
		t.Fatalf("limits %v", q.limits)
	}
}

func TestEnsureSlotOwnership(t *testing.T) {
	p, _ := setup(t)
	if err := p.Ensure(uidA, 5); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(uidB, 5); !errors.Is(err, ErrConflict) {
		t.Fatalf("other uid took slot 5: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(p.Base, uidB)); !os.IsNotExist(err) {
		t.Fatal("refused ensure must not create the dir")
	}
	if err := p.Ensure(uidA, 6); !errors.Is(err, ErrConflict) {
		t.Fatalf("uid moved to another slot: %v", err)
	}
}

func TestEnsureRefusesSymlinks(t *testing.T) {
	p, _ := setup(t)
	victim := t.TempDir()
	// symlinked person dir
	if err := os.Symlink(victim, filepath.Join(p.Base, uidA)); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(uidA, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("symlinked top: %v", err)
	}
	// symlinked subdir, as the container could plant one
	if err := os.Mkdir(filepath.Join(p.Base, uidB), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(p.Base, uidB, "config")); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(uidB, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("symlinked subdir: %v", err)
	}
	// regular file instead of a dir
	os.Remove(filepath.Join(p.Base, uidB, "config"))
	os.WriteFile(filepath.Join(p.Base, uidB, "config"), nil, 0o600)
	if err := p.Ensure(uidB, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("file subdir: %v", err)
	}
	fi, _ := os.Stat(victim)
	if st := fi.Sys().(*syscall.Stat_t); st.Uid == Owner {
		t.Fatal("victim was chowned")
	}
}

func TestEnsureRefusesForeignOwner(t *testing.T) {
	p, _ := setup(t)
	d := filepath.Join(p.Base, uidA)
	os.Mkdir(d, 0o750)
	os.Chown(d, 4242, 4242)
	if err := p.Ensure(uidA, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign owner: %v", err)
	}
}

func TestHTTP(t *testing.T) {
	p, _ := setup(t)
	sock := filepath.Join(t.TempDir(), "sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: Handler(p, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	c := NewClient(sock)
	ctx := context.Background()
	if err := c.Ensure(ctx, uidA, 7); err != nil {
		t.Fatal(err)
	}
	if err := c.Ensure(ctx, "nope", 7); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatal(err)
	}
	if err := c.Ensure(ctx, uidB, 7); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatal(err)
	}

	if err := c.Purge(ctx, uidA, 8); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("purge wrong slot: %v", err)
	}
	if err := c.Purge(ctx, "nope", 7); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("purge invalid: %v", err)
	}
	if used, limit, err := c.Usage(ctx, 7); err != nil || used != 1e6 || limit != 20<<30 {
		t.Fatalf("usage slot 7: %d %d %v", used, limit, err)
	}
	if used, limit, err := c.Usage(ctx, 8); err != nil || used != 0 || limit != 0 {
		t.Fatalf("unknown project: %d %d %v", used, limit, err)
	}
	for _, bad := range []string{"0", "255", "-1", "+7", "07", "7.0", "x", "", "7&slot=8", "1000000000000000000000", "7%00"} {
		resp, err := c.h.Get("http://oc-provision/usage?slot=" + bad)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("slot=%q: %s", bad, resp.Status)
		}
	}
	if resp, err := c.h.Get("http://oc-provision/usage"); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing slot: %v %v", resp, err)
	}
	if err := c.Purge(ctx, uidA, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(p.Base, uidA)); !os.IsNotExist(err) {
		t.Fatalf("purge over http left the dir: %v", err)
	}
}

func TestPurge(t *testing.T) {
	p, q := setup(t)
	if err := p.Ensure(uidA, 3); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(uidB, 4); err != nil {
		t.Fatal(err)
	}
	top := filepath.Join(p.Base, uidA)
	victim := t.TempDir()
	os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600)
	os.MkdirAll(filepath.Join(top, "workspace", "p", "a", "b"), 0o755)
	os.WriteFile(filepath.Join(top, "workspace", "p", "a", "b", "f.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(top, "data", ".hidden"), []byte("x"), 0o600)
	// links planted by the container: to a dir outside, to a file outside, to the other person
	os.Symlink(victim, filepath.Join(top, "workspace", "p", "out"))
	os.Symlink(filepath.Join(victim, "keep"), filepath.Join(top, "config", "keep"))
	os.Symlink(filepath.Join(p.Base, uidB), filepath.Join(top, "state", "other"))
	for i := range 600 { // more than one Readdirnames batch
		os.WriteFile(filepath.Join(top, "trash", strconv.Itoa(i)), nil, 0o600)
	}

	for _, c := range []struct {
		name string
		uid  string
		slot int
		want error
	}{
		{"wrong slot", uidA, 4, ErrConflict},
		{"other slot of B", uidB, 3, ErrConflict},
		{"invalid uid", "../" + uidA[3:], 3, ErrInvalid},
		{"invalid slot", uidA, 0, ErrInvalid},
	} {
		if err := p.Purge(c.uid, c.slot); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(top, "workspace", "p", "a", "b", "f.txt")); err != nil {
		t.Fatal("refused purge removed data")
	}

	if err := p.Purge(uidA, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(top); !os.IsNotExist(err) {
		t.Fatalf("top still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Fatal("symlink target outside was removed")
	}
	if _, err := os.Stat(filepath.Join(p.Base, uidB, "workspace")); err != nil {
		t.Fatal("other person's data was removed")
	}
	if err := p.Purge(uidA, 3); err != nil { // idempotent
		t.Fatal(err)
	}
	if len(q.cleared) != 2 || q.cleared[0] != 10003 {
		t.Fatalf("cleared %v", q.cleared)
	}
	// the slot is free again for a new person
	if err := p.Ensure(uidA[:63]+"f", 3); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeRefuses(t *testing.T) {
	p, q := setup(t)
	victim := t.TempDir()
	os.WriteFile(filepath.Join(victim, "keep"), []byte("x"), 0o600)
	// symlinked person dir
	os.Symlink(victim, filepath.Join(p.Base, uidA))
	if err := p.Purge(uidA, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("symlinked top: %v", err)
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Fatal("followed the symlink")
	}
	// foreign owner
	d := filepath.Join(p.Base, uidB)
	os.Mkdir(d, 0o750)
	q.ids[inoPath(t, d)] = 10002
	os.Chown(d, 4242, 4242)
	if err := p.Purge(uidB, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign owner: %v", err)
	}
	// right owner, no project id (never ensured)
	os.Chown(d, Owner, Owner)
	q.ids[inoPath(t, d)] = 0
	if err := p.Purge(uidB, 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("no project id: %v", err)
	}
	if _, err := os.Stat(d); err != nil {
		t.Fatal("refused purge removed the dir")
	}
	if len(q.cleared) != 0 {
		t.Fatalf("refused purge cleared limits %v", q.cleared)
	}
}

// A uid without a dir must not clear the limit of a slot another person holds.
func TestPurgeNonexistentUIDKeepsOtherLimit(t *testing.T) {
	p, q := setup(t)
	if err := p.Ensure(uidA, 3); err != nil {
		t.Fatal(err)
	}
	if err := p.Purge(uidB, 3); !errors.Is(err, ErrConflict) {
		t.Fatalf("purge of a foreign slot: %v", err)
	}
	if len(q.cleared) != 0 {
		t.Fatalf("cleared %v", q.cleared)
	}
	if err := p.Purge(uidB, 4); err != nil || len(q.cleared) != 1 || q.cleared[0] != 10004 {
		t.Fatalf("free slot: %v %v", err, q.cleared)
	}
}

func inoPath(t *testing.T, path string) uint64 {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}
