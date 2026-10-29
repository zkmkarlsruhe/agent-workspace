// SPDX-License-Identifier: MIT

// Package provision creates a person's data directory with its xfs project
// quota (D33, docs/adr/README.md §3.10 as adjusted by D44). The root side runs in
// cmd/oc-provision; the manager only calls Ensure over the unix socket.
package provision

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/zkmkarlsruhe/agent-workspace/manager/internal/config"
)

const (
	// Owner is container uid/gid 1000 under the shared userns-remap range (D44).
	Owner = 101000
	// ProjBase + slot is the xfs project id of a person's tree.
	ProjBase = 10000
	dirMode  = 0o750
)

// Subdirs are mounted by the manager (lifecycle spec) plus trash (docs/adr/README.md §3.10).
var Subdirs = []string{"workspace", "config", "data", "state", "trash"}

var (
	ErrInvalid  = errors.New("invalid request")
	ErrConflict = errors.New("conflict")
	uidRE       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Quota is the root-only part, faked in tests.
type Quota interface {
	ProjID(fd int) (uint32, error)
	SetProjID(fd int, id uint32) error
	SetLimit(id uint32) error
	// ClearLimit drops the limits of a project (account deletion).
	ClearLimit(id uint32) error
	// Usage reports bytes used and the hard limit of a project; an unknown project is 0, 0.
	Usage(id uint32) (used, limit int64, err error)
}

type Provisioner struct {
	Base string // e.g. /srv/opencode/users, trusted (root config)
	Q    Quota
	mu   sync.Mutex // serialises the slot check against concurrent ensures
}

func Validate(uid string, slot int) error {
	if !uidRE.MatchString(uid) {
		return fmt.Errorf("%w: uid must be 64 lowercase hex", ErrInvalid)
	}
	return ValidSlot(slot)
}

func ValidSlot(slot int) error {
	if slot < 1 || slot > config.MaxSlot {
		return fmt.Errorf("%w: slot must be in [1, %d]", ErrInvalid, config.MaxSlot)
	}
	return nil
}

// Usage reads the quota of slot's project. Read-only, no path is touched.
func (p *Provisioner) Usage(slot int) (used, limit int64, err error) {
	if err := ValidSlot(slot); err != nil {
		return 0, 0, err
	}
	return p.Q.Usage(uint32(ProjBase + slot))
}

// Ensure creates Base/<uid>/{Subdirs} owned by Owner with project id
// ProjBase+slot and its limit. Idempotent. Every path component is opened
// relative to its parent fd with O_NOFOLLOW, so a symlink planted by the
// container (it owns the tree) is refused, never followed.
func (p *Provisioner) Ensure(uid string, slot int) error {
	if err := Validate(uid, slot); err != nil {
		return err
	}
	want := uint32(ProjBase + slot)
	p.mu.Lock()
	defer p.mu.Unlock()

	base, err := unix.Open(p.Base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", p.Base, err)
	}
	defer unix.Close(base)
	if err := p.slotFree(base, uid, want); err != nil {
		return err
	}

	top, err := ensureDir(base, uid)
	if err != nil {
		return err
	}
	defer unix.Close(top)
	if err := p.setProj(top, uid, want); err != nil {
		return err
	}
	for _, name := range Subdirs {
		fd, err := ensureDir(top, name)
		if err != nil {
			return fmt.Errorf("%s/%s: %w", uid, name, err)
		}
		err = p.setProj(fd, uid+"/"+name, want)
		unix.Close(fd)
		if err != nil {
			return err
		}
	}
	return p.Q.SetLimit(want)
}

// slotFree refuses when another person's directory already carries the slot's project id.
func (p *Provisioner) slotFree(base int, uid string, want uint32) error {
	dup, err := unix.Dup(base)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(dup), p.Base)
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, n := range names {
		if n == uid || !uidRE.MatchString(n) {
			continue
		}
		fd, err := unix.Openat(base, n, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			continue // not a directory; ensure never creates such entries
		}
		id, err := p.Q.ProjID(fd)
		unix.Close(fd)
		if err != nil {
			return fmt.Errorf("project id of %s: %w", n, err)
		}
		if id == want {
			return fmt.Errorf("%w: project %d belongs to %s", ErrConflict, want, n)
		}
	}
	return nil
}

func (p *Provisioner) setProj(fd int, name string, want uint32) error {
	id, err := p.Q.ProjID(fd)
	if err != nil {
		return fmt.Errorf("project id of %s: %w", name, err)
	}
	switch id {
	case want:
		return nil
	case 0:
		return p.Q.SetProjID(fd, want)
	}
	return fmt.Errorf("%w: %s has project %d, not %d", ErrConflict, name, id, want)
}

// ensureDir creates name below dir if missing and returns an fd on it after
// checking it is a real directory owned by root or Owner, then fixes owner and mode.
func ensureDir(dir int, name string) (int, error) {
	if err := unix.Mkdirat(dir, name, dirMode); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, fmt.Errorf("mkdir %s: %w", name, err)
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return -1, fmt.Errorf("%w: %s is not a plain directory", ErrConflict, name)
	}
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", name, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if st.Uid != 0 && st.Uid != Owner {
		unix.Close(fd)
		return -1, fmt.Errorf("%w: %s owned by uid %d", ErrConflict, name, st.Uid)
	}
	if err := unix.Fchown(fd, Owner, Owner); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("chown %s: %w", name, err)
	}
	if err := unix.Fchmod(fd, dirMode); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("chmod %s: %w", name, err)
	}
	return fd, nil
}

// Purge removes Base/<uid> with everything below it and clears the limit of
// project ProjBase+slot (account deletion). Idempotent: a missing dir only
// clears the limit. The top dir must be a plain directory owned by root or
// Owner and carry exactly the slot's project id, so a wrong uid/slot pair
// never deletes another person's data. Removal walks fds with O_NOFOLLOW:
// symlinks planted by the container are unlinked, never followed, and a
// directory on another device (a mount) stops the purge.
func (p *Provisioner) Purge(uid string, slot int) error {
	if err := Validate(uid, slot); err != nil {
		return err
	}
	want := uint32(ProjBase + slot)
	p.mu.Lock()
	defer p.mu.Unlock()

	base, err := unix.Open(p.Base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", p.Base, err)
	}
	defer unix.Close(base)
	top, err := unix.Openat(base, uid, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return p.clearLimit(base, uid, want)
	case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR):
		return fmt.Errorf("%w: %s is not a plain directory", ErrConflict, uid)
	case err != nil:
		return fmt.Errorf("open %s: %w", uid, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(top, &st); err != nil {
		unix.Close(top)
		return err
	}
	if st.Uid != 0 && st.Uid != Owner {
		unix.Close(top)
		return fmt.Errorf("%w: %s owned by uid %d", ErrConflict, uid, st.Uid)
	}
	id, err := p.Q.ProjID(top)
	if err == nil && id != want {
		err = fmt.Errorf("%w: %s has project %d, not %d", ErrConflict, uid, id, want)
	}
	if err == nil {
		err = removeContents(top, st.Dev)
	}
	unix.Close(top)
	if err != nil {
		return fmt.Errorf("purge %s: %w", uid, err)
	}
	if err := unix.Unlinkat(base, uid, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("rmdir %s: %w", uid, err)
	}
	return p.clearLimit(base, uid, want)
}

// clearLimit drops the limit only if no other person's dir carries the
// project id: a purge with a uid that has no dir must not lift another
// slot's quota.
func (p *Provisioner) clearLimit(base int, uid string, want uint32) error {
	if err := p.slotFree(base, uid, want); err != nil {
		return err
	}
	return p.Q.ClearLimit(want)
}

// removeContents empties the directory dir. Entries are opened relative to
// dir with O_NOFOLLOW; anything that is not a directory is unlinked.
// shortcut: one fd per level, so a tree deeper than the fd limit fails the
// purge (EMFILE, retry-safe); walk iteratively if that ever happens.
func removeContents(dir int, dev uint64) error {
	dup, err := unix.Dup(dir)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	for {
		names, err := f.Readdirnames(256)
		for _, n := range names {
			if err := removeEntry(dir, n, dev); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(names) == 0) {
			return nil
		}
		if err != nil {
			return err
		}
		// entries were removed under the open stream: restart from the top
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
}

func removeEntry(dir int, name string, dev uint64) error {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return unix.Unlinkat(dir, name, 0) // file, symlink, fifo, socket: the entry itself goes
	}
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open entry: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return err
	}
	if st.Dev != dev {
		unix.Close(fd)
		return fmt.Errorf("%w: mount point inside the tree", ErrConflict)
	}
	err = removeContents(fd, dev)
	unix.Close(fd)
	if err != nil {
		return err
	}
	return unix.Unlinkat(dir, name, unix.AT_REMOVEDIR)
}
