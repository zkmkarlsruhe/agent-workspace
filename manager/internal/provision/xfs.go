// SPDX-License-Identifier: MIT

package provision

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fsxattr mirrors struct fsxattr from linux/fs.h; x/sys has no wrapper for it.
type fsxattr struct {
	Xflags     uint32
	Extsize    uint32
	Nextents   uint32
	Projid     uint32
	Cowextsize uint32
	Pad        [8]byte
}

// _IOR/_IOW('X', 31/32, struct fsxattr) on amd64 and arm64.
const (
	fsIocFsgetxattr    = 0x801c581f
	fsIocFssetxattr    = 0x401c5820
	fsXflagProjinherit = 0x200
	// QCMD(Q_GETQUOTA / Q_SETQUOTA, PRJQUOTA) from linux/quota.h
	qGetPrjQuota = 0x800007<<8 | 2
	qSetPrjQuota = 0x800008<<8 | 2
	qifLimits    = 1 | 4 // QIF_BLIMITS | QIF_ILIMITS
	// D38: 20 GiB per person; docs/adr/README.md §3.10. Block limits in 1 KiB units.
	bhardKiB, bsoftKiB, ihard = 20 << 20, 18 << 20, 2000000
)

// ifDqblk mirrors struct if_dqblk from linux/quota.h.
type ifDqblk struct {
	Bhardlimit, Bsoftlimit, Curspace  uint64 // limits in 1 KiB blocks, Curspace in bytes
	Ihardlimit, Isoftlimit, Curinodes uint64
	Btime, Itime                      uint64
	Valid                             uint32
}

// XFS sets project ids by ioctl on the fd and limits via quotactl_fd on the
// mount point (Linux >= 5.14): no xfs_quota binary, no block device path.
type XFS struct {
	Mount string // mount point of the prjquota filesystem, e.g. /srv/opencode
}

func (XFS) ProjID(fd int) (uint32, error) {
	var a fsxattr
	if err := ioctl(fd, fsIocFsgetxattr, &a); err != nil {
		return 0, err
	}
	return a.Projid, nil
}

// SetProjID also sets PROJINHERIT so new files below inherit the id.
func (XFS) SetProjID(fd int, id uint32) error {
	var a fsxattr
	if err := ioctl(fd, fsIocFsgetxattr, &a); err != nil {
		return err
	}
	a.Projid = id
	a.Xflags |= fsXflagProjinherit
	return ioctl(fd, fsIocFssetxattr, &a)
}

func (x XFS) SetLimit(id uint32) error {
	return x.setQuota(id, &ifDqblk{Bhardlimit: bhardKiB, Bsoftlimit: bsoftKiB, Ihardlimit: ihard, Valid: qifLimits})
}

// ClearLimit sets all limits of the project to 0 (= none); the dquot goes once it is empty.
func (x XFS) ClearLimit(id uint32) error {
	return x.setQuota(id, &ifDqblk{Valid: qifLimits})
}

// Usage reports bytes used and the hard limit; an unknown project is 0, 0.
func (x XFS) Usage(id uint32) (used, limit int64, err error) {
	var q ifDqblk
	e := x.quotactl(qGetPrjQuota, id, &q)
	if e == unix.ENOENT {
		return 0, 0, nil // no dquot yet: nothing written, no limit set
	}
	if e != nil {
		return 0, 0, fmt.Errorf("quotactl get project %d: %w", id, e)
	}
	return int64(q.Curspace), int64(q.Bhardlimit) * 1024, nil
}

func (x XFS) setQuota(id uint32, q *ifDqblk) error {
	if e := x.quotactl(qSetPrjQuota, id, q); e != nil {
		return fmt.Errorf("quotactl set project %d: %w", id, e)
	}
	return nil
}

// quotactl runs quotactl_fd(2) on the mount point; a kernel error comes back as a bare unix.Errno.
func (x XFS) quotactl(cmd uintptr, id uint32, q *ifDqblk) error {
	fd, err := unix.Open(x.Mount, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", x.Mount, err) // wrapped: Usage must not read a missing mount as ENOENT
	}
	defer unix.Close(fd)
	if e := sysQuotactlFd(fd, cmd, id, q); e != 0 {
		return e
	}
	return nil
}

// sysQuotactlFd is swapped in tests.
var sysQuotactlFd = func(fd int, cmd uintptr, id uint32, q *ifDqblk) unix.Errno {
	_, _, e := unix.Syscall6(unix.SYS_QUOTACTL_FD, uintptr(fd), cmd, uintptr(id), uintptr(unsafe.Pointer(q)), 0, 0)
	return e
}

func ioctl(fd int, req uintptr, a *fsxattr) error {
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(a))); e != 0 {
		return e
	}
	return nil
}
