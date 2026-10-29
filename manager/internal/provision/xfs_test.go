// SPDX-License-Identifier: MIT

package provision

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

type qcall struct {
	cmd uintptr
	id  uint32
	q   ifDqblk
}

// fakeQuotactl records calls and answers with ret (filled into q for gets).
func fakeQuotactl(t *testing.T, ret ifDqblk, errno unix.Errno) *[]qcall {
	t.Helper()
	var calls []qcall
	old := sysQuotactlFd
	sysQuotactlFd = func(fd int, cmd uintptr, id uint32, q *ifDqblk) unix.Errno {
		calls = append(calls, qcall{cmd, id, *q})
		if cmd == qGetPrjQuota && errno == 0 {
			*q = ret
		}
		return errno
	}
	t.Cleanup(func() { sysQuotactlFd = old })
	return &calls
}

func TestQuotactlCommands(t *testing.T) {
	// QCMD(Q_GETQUOTA, PRJQUOTA) and QCMD(Q_SETQUOTA, PRJQUOTA), linux/quota.h
	if qGetPrjQuota != 0x80000702 || qSetPrjQuota != 0x80000802 {
		t.Fatalf("cmds %#x %#x", qGetPrjQuota, qSetPrjQuota)
	}
}

func TestSetAndClearLimit(t *testing.T) {
	calls := fakeQuotactl(t, ifDqblk{}, 0)
	x := XFS{Mount: t.TempDir()}
	if err := x.SetLimit(10003); err != nil {
		t.Fatal(err)
	}
	if err := x.ClearLimit(10003); err != nil {
		t.Fatal(err)
	}
	want := []qcall{
		{qSetPrjQuota, 10003, ifDqblk{Bhardlimit: 20 << 20, Bsoftlimit: 18 << 20, Ihardlimit: 2000000, Valid: 5}},
		{qSetPrjQuota, 10003, ifDqblk{Valid: 5}},
	}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Fatalf("calls %+v", *calls)
	}
}

func TestUsage(t *testing.T) {
	fakeQuotactl(t, ifDqblk{Curspace: 12345, Bhardlimit: 20 << 20}, 0)
	used, limit, err := XFS{Mount: t.TempDir()}.Usage(10001)
	if err != nil || used != 12345 || limit != 20<<30 {
		t.Fatalf("%d %d %v", used, limit, err)
	}
}

func TestUsageNoDquot(t *testing.T) {
	fakeQuotactl(t, ifDqblk{}, unix.ENOENT)
	used, limit, err := XFS{Mount: t.TempDir()}.Usage(10001)
	if err != nil || used != 0 || limit != 0 {
		t.Fatalf("%d %d %v", used, limit, err)
	}
}

func TestQuotactlErrors(t *testing.T) {
	fakeQuotactl(t, ifDqblk{}, unix.EPERM)
	x := XFS{Mount: t.TempDir()}
	if err := x.SetLimit(1); !errors.Is(err, unix.EPERM) {
		t.Fatalf("SetLimit: %v", err)
	}
	if _, _, err := x.Usage(1); !errors.Is(err, unix.EPERM) {
		t.Fatalf("Usage: %v", err)
	}
	// a missing mount point is an error, never "no dquot"
	calls := fakeQuotactl(t, ifDqblk{}, 0)
	if _, _, err := (XFS{Mount: filepath.Join(t.TempDir(), "gone")}).Usage(1); !errors.Is(err, unix.ENOENT) || len(*calls) != 0 {
		t.Fatalf("missing mount: %v, %d calls", err, len(*calls))
	}
}

// TestRealXFS runs against a real prjquota xfs as root:
// OC_XFS_TEST_MOUNT=/mnt/x go test -run RealXFS ./internal/provision/
func TestRealXFS(t *testing.T) {
	mnt := os.Getenv("OC_XFS_TEST_MOUNT")
	if mnt == "" {
		t.Skip("OC_XFS_TEST_MOUNT not set")
	}
	const id = 60001
	x := XFS{Mount: mnt}
	dir, err := os.MkdirTemp(mnt, "oc-xfs-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := x.SetProjID(fd, id); err != nil {
		t.Fatal(err)
	}
	if got, err := x.ProjID(fd); err != nil || got != id {
		t.Fatalf("project id %d %v", got, err)
	}
	if err := x.SetLimit(id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	unix.Sync()
	used, limit, err := x.Usage(id)
	if err != nil || used < 1<<20 || limit != 20<<30 {
		t.Fatalf("after SetLimit: used %d limit %d err %v", used, limit, err)
	}
	if err := x.ClearLimit(id); err != nil {
		t.Fatal(err)
	}
	if _, limit, err := x.Usage(id); err != nil || limit != 0 {
		t.Fatalf("after ClearLimit: limit %d err %v", limit, err)
	}
}
