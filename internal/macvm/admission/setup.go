// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// SetupConfig configures Setup, the installer's half of the ledger. The
// package postinstall runs it as root (wardyn-macvm ledger-setup) once its
// shell has made the base directory with mkdir, or found one there.
type SetupConfig struct {
	Base     string        // the base directory, DefaultLockPath's parent
	LockWait time.Duration // 0: LockWait
	// Fstat and Chown replace fstat(2) and fchown(fd, 0, 0) (tests).
	Fstat func(fd int, name string) (unix.Stat_t, error)
	Chown func(f *os.File) error

	afterFlock func() // tests: runs once the old lock's flock is held
}

const entriesName = "macos-vm-admission"

// Setup makes the ledger under Base exact, and leaves an exact one as it is:
//
//	Base                     root:wheel 0755
//	Base/admission.lock      root:wheel 0644, a regular file
//	Base/macos-vm-admission  root:wheel 1777
//
// It walks to Base from / as the helper does. Before it changes anything it
// takes the existing lock's flock, waiting LockWait at most, and holds it to
// the end, so no helper is inside an admission while the lock or the entries
// move; a helper that opened the old lock refuses at its recheck once it gets
// the flock. Nothing found already there is chowned or chmodded: a base that
// is not exact is moved into a quarantine beside it and made again, a child
// that is not exact is replaced, and every entry moves to the new entries
// directory. It touches nothing outside Base and its .wardyn-quarantine.*
// siblings.
func Setup(c SetupConfig) error {
	if c.LockWait == 0 {
		c.LockWait = LockWait
	}
	if c.Chown == nil {
		c.Chown = func(f *os.File) error { return f.Chown(0, 0) }
	}
	s := &setup{c: c, l: &Ledger{c: Config{Dir: filepath.Join(c.Base, entriesName), Fstat: c.Fstat, LockWait: c.LockWait}}}
	base, err := s.l.walkDir(c.Base)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(base) }()
	st, err := s.l.fstat(base, c.Base)
	if err != nil {
		return err
	}
	if uint32(st.Mode)&groupOtherWrite != 0 {
		return notInstalled("%s is writable by other users", c.Base)
	}
	s.id = [2]uint64{uint64(st.Dev), st.Ino}
	old, keepLock, err := s.lockOld(base)
	if err != nil {
		return err
	}
	if old != nil {
		defer old.Close()
	}
	from := c.Base
	if !exact(st, unix.S_IFDIR, 0o755) {
		if from, err = s.rebuildBase(); err != nil {
			return err
		}
		keepLock = false
	}
	if err := s.entries(from); err != nil {
		return err
	}
	if !keepLock {
		if err := s.newLock(); err != nil {
			return err
		}
	}
	return s.verify()
}

type setup struct {
	c  SetupConfig
	l  *Ledger
	id [2]uint64 // the base's dev and inode
}

// exact: the type, root:wheel and exactly that mode.
func exact(st unix.Stat_t, typ, perm uint32) bool {
	m := uint32(st.Mode)
	return st.Uid == 0 && st.Gid == 0 && m&unix.S_IFMT == typ && m&0o7777 == perm
}

// lockOld takes the flock of the lock in base, if one is there as a regular
// file, and says whether it is exact. The caller closes the returned file (nil
// when there is none), which drops the flock.
func (s *setup) lockOld(base int) (*os.File, bool, error) {
	path := filepath.Join(s.c.Base, lockFile)
	fd, err := unix.Openat(base, lockFile, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, nil // nothing a helper could hold
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := s.l.fstat(fd, path)
	if err != nil || uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG {
		return f, false, nil
	}
	if err := flockWait(f, s.c.LockWait); err != nil {
		f.Close()
		return nil, false, fmt.Errorf("admission: a helper holds %s: %w", path, err)
	}
	if s.c.afterFlock != nil {
		s.c.afterFlock()
	}
	var now unix.Stat_t
	if err := unix.Fstatat(base, lockFile, &now, unix.AT_SYMLINK_NOFOLLOW); err != nil || now.Dev != st.Dev || now.Ino != st.Ino {
		f.Close()
		return nil, false, notInstalled("%s changed while the installer locked it", path)
	}
	return f, exact(st, unix.S_IFREG, 0o644), nil
}

// quarantine moves path into a new root-only directory beside it.
func (s *setup) quarantine(path string) error {
	q, err := os.MkdirTemp(filepath.Dir(path), ".wardyn-quarantine.")
	if err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(q, filepath.Base(path)))
}

// rebuildBase moves the base into a quarantine and makes it again with mkdir,
// three tries at most if something is planted at the name meanwhile. It
// returns where the old base now is.
func (s *setup) rebuildBase() (string, error) {
	q, err := os.MkdirTemp(filepath.Dir(s.c.Base), ".wardyn-quarantine.")
	if err != nil {
		return "", err
	}
	from := filepath.Join(q, filepath.Base(s.c.Base))
	if err := os.Rename(s.c.Base, from); err != nil {
		return "", err
	}
	for i := 0; ; i++ {
		err := s.made(os.Mkdir(s.c.Base, 0o755), s.c.Base, 0o755)
		if err == nil {
			var st unix.Stat_t
			err = unix.Lstat(s.c.Base, &st)
			s.id = [2]uint64{uint64(st.Dev), st.Ino}
		}
		if err == nil || i == 2 || !errors.Is(err, os.ErrExist) {
			return from, err
		}
		if err := s.quarantine(s.c.Base); err != nil {
			return "", err
		}
	}
}

// made gives a directory this run's mkdir just made its owner and mode.
func (s *setup) made(mkErr error, path string, perm os.FileMode) error {
	if mkErr != nil {
		return mkErr
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := s.c.Chown(f); err != nil {
		return err
	}
	return f.Chmod(perm)
}

// entries makes Base's entries directory exact, moving every entry from
// from's entries directory (from is Base itself, or the quarantined old base).
func (s *setup) entries(from string) error {
	dst := filepath.Join(s.c.Base, entriesName)
	src := filepath.Join(from, entriesName)
	if from == s.c.Base && s.isExact(dst, unix.S_IFDIR, 0o1777) {
		return nil
	}
	tmp, err := os.MkdirTemp(s.c.Base, "."+entriesName+".")
	if err != nil {
		return err
	}
	if err := s.made(nil, tmp, os.ModeSticky|0o777); err != nil {
		return err
	}
	if err := moveEntries(src, tmp); err != nil {
		return err
	}
	if from == s.c.Base {
		if _, err := os.Lstat(src); err == nil {
			if err := s.quarantine(src); err != nil {
				return err
			}
		}
	}
	return os.Rename(tmp, dst)
}

// moveEntries renames every entry of src, if src is a real directory, into dst.
func moveEntries(src, dst string) error {
	d, err := os.OpenFile(src, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil // no directory there (missing, a link or a file): no entries
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, n := range names {
		if err := os.Rename(filepath.Join(src, n), filepath.Join(dst, n)); err != nil {
			return err
		}
	}
	return nil
}

// newLock replaces the lock with a new exact one, in one rename; anything
// there that is not a regular file is moved aside first.
func (s *setup) newLock() error {
	lock := filepath.Join(s.c.Base, lockFile)
	if fi, err := os.Lstat(lock); err == nil && !fi.Mode().IsRegular() {
		if err := s.quarantine(lock); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(s.c.Base, ".admission.lock.")
	if err != nil {
		return err
	}
	defer f.Close()
	if err := s.c.Chown(f); err != nil {
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), lock)
}

// isExact opens path without following a link and checks its one fstat.
func (s *setup) isExact(path string, typ, perm uint32) bool {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	st, err := s.l.fstat(fd, path)
	return err == nil && exact(st, typ, perm)
}

// verify walks to Base again, checks it is the directory set up (dev and
// inode), and checks the whole layout.
func (s *setup) verify() error {
	base, err := s.l.walkDir(s.c.Base)
	if err != nil {
		return err
	}
	st, err := s.l.fstat(base, s.c.Base)
	_ = unix.Close(base)
	switch {
	case err != nil:
		return err
	case [2]uint64{uint64(st.Dev), st.Ino} != s.id:
		return notInstalled("%s changed while the installer set it up", s.c.Base)
	case !exact(st, unix.S_IFDIR, 0o755):
		return notInstalled("%s is not root:wheel 0755", s.c.Base)
	case !s.isExact(filepath.Join(s.c.Base, lockFile), unix.S_IFREG, 0o644):
		return notInstalled("%s is not a root:wheel 0644 file", filepath.Join(s.c.Base, lockFile))
	case !s.isExact(filepath.Join(s.c.Base, entriesName), unix.S_IFDIR, 0o1777):
		return notInstalled("%s is not root:wheel 1777", filepath.Join(s.c.Base, entriesName))
	}
	return nil
}
