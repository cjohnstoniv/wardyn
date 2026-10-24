// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

	afterFlock func()                 // tests: runs once the old lock's flock is held
	beforeMove func(dst string) error // tests: runs before each entry is moved
}

const (
	entriesName = "macos-vm-admission"
	sockName    = "sock" // the logins' socket directories, sock/u<uid>/
)

// Setup makes the ledger under Base exact, and leaves an exact one as it is:
//
//	Base                     root:wheel 0755
//	Base/admission.lock      root:wheel 0644, a regular file
//	Base/macos-vm-admission  root:wheel 1777
//	Base/sock                root:wheel 1777, holding only u<uid>: uid's 0700 directory
//
// It walks to Base from / as the helper does. Before it changes anything it
// takes the existing lock's flock, waiting LockWait at most, and holds it to
// the end, so no helper is inside an admission while the lock or the entries
// move; a helper that opened the old lock refuses at its recheck once it gets
// the flock. Nothing found already there is chowned or chmodded: a base that
// is not exact is moved into a quarantine beside it and made again, a child
// that is not exact is replaced, and every entry it can find (in the entries
// directory, in a staging directory or quarantine an earlier run left) moves
// to the new entries directory, or it refuses: it never ends with fewer
// entries than it found. It touches nothing outside Base and its
// .wardyn-quarantine.* siblings.
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
	if err := s.entries(); err != nil {
		return err
	}
	if err := s.sock(from); err != nil {
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
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil // nothing a helper could hold
	}
	if err != nil { // a link, a socket, or an open that failed: never replaced unlocked
		return nil, false, notInstalled("%s cannot be opened (%v)", path, err)
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
func (s *setup) quarantine(path string) error { return quarantineIn(filepath.Dir(path), path) }

// quarantineIn moves path into a new root-only directory in dir.
func quarantineIn(dir, path string) error {
	q, err := os.MkdirTemp(dir, ".wardyn-quarantine.")
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

// entries makes Base's entries directory exact. Unless it already is and no
// entry is anywhere else, every entry found moves into a staging directory
// that stays root-only 0700 until all are in it and counted, which then
// becomes 1777 and takes the final name in one rename.
func (s *setup) entries() error {
	dst := filepath.Join(s.c.Base, entriesName)
	srcs, names, total, err := s.entrySources()
	if err != nil {
		return err
	}
	if s.isExact(dst, unix.S_IFDIR, 0o1777) && len(names[dst]) == total {
		return nil
	}
	tmp, err := os.MkdirTemp(s.c.Base, "."+entriesName+".")
	if err != nil {
		return err
	}
	for _, src := range srcs {
		if err := s.moveAll(src, names[src], tmp); err != nil {
			return err
		}
	}
	if got, err := readNames(tmp); err != nil || len(got) != total {
		return notInstalled("moving the macOS VM entries found %d of %d (%v)", len(got), total, err)
	}
	if _, err := os.Lstat(dst); err == nil {
		if err := quarantineIn(s.c.Base, dst); err != nil {
			return err
		}
	}
	if err := s.made(nil, tmp, os.ModeSticky|0o777); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	for _, src := range srcs[1:] {
		_ = os.Remove(src) // an emptied staging directory; a quarantine's stays
	}
	return nil
}

// entrySources lists every root-owned directory that may hold entries, Base's
// entries directory first: staging directories a failed run left, and the
// entries directories in earlier quarantines, in Base and beside it (a rebuilt
// base's). It returns each one's entries and their total.
func (s *setup) entrySources() ([]string, map[string][]string, int, error) {
	dirs := []string{filepath.Join(s.c.Base, entriesName)}
	for _, pat := range []string{
		filepath.Join(s.c.Base, "."+entriesName+".*"),
		filepath.Join(s.c.Base, ".wardyn-quarantine.*", entriesName),
		filepath.Join(filepath.Dir(s.c.Base), ".wardyn-quarantine.*", filepath.Base(s.c.Base), entriesName),
	} {
		m, _ := filepath.Glob(pat)
		dirs = append(dirs, m...)
	}
	names := map[string][]string{}
	var srcs []string
	total := 0
	for i, d := range dirs {
		if i > 0 && !s.rootDir(d) {
			continue // a login's (a planted base, quarantined): not entries
		}
		n, err := readNames(d)
		if err != nil {
			return nil, nil, 0, err
		}
		srcs, names[d] = append(srcs, d), n
		total += len(n)
	}
	return srcs, names, total, nil
}

// rootDir: path and its parent are root-owned real directories.
func (s *setup) rootDir(path string) bool {
	for _, p := range []string{path, filepath.Dir(path)} {
		fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return false
		}
		st, err := s.l.fstat(fd, p)
		_ = unix.Close(fd)
		if err != nil || st.Uid != 0 {
			return false
		}
	}
	return true
}

// readNames lists dir if it is a real directory; anything else has none.
func readNames(dir string) ([]string, error) {
	d, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil
	}
	defer d.Close()
	return d.Readdirnames(-1)
}

// moveAll renames each of names from src into dst, never over something there.
func (s *setup) moveAll(src string, names []string, dst string) error {
	for _, n := range names {
		to := filepath.Join(dst, n)
		if _, err := os.Lstat(to); err == nil {
			return notInstalled("two macOS VM entries are named %s (%s)", n, src)
		}
		if s.c.beforeMove != nil {
			if err := s.c.beforeMove(to); err != nil {
				return err
			}
		}
		if err := os.Rename(filepath.Join(src, n), to); err != nil {
			return err
		}
	}
	return nil
}

// sock makes Base/sock exact: a sound one keeps its sound u<uid> directories
// and loses the rest to a quarantine; otherwise the sound ones move from
// from's sock into a new one, built root-only as entries is.
func (s *setup) sock(from string) error {
	dst := filepath.Join(s.c.Base, sockName)
	if from == s.c.Base && s.isExact(dst, unix.S_IFDIR, 0o1777) {
		return s.sweepSock(dst)
	}
	tmp, err := os.MkdirTemp(s.c.Base, "."+sockName+".")
	if err != nil {
		return err
	}
	src := filepath.Join(from, sockName)
	names, err := readNames(src)
	if err != nil {
		return err
	}
	var keep []string
	for _, n := range names {
		if s.sockChild(src, n) {
			keep = append(keep, n)
		}
	}
	if err := s.moveAll(src, keep, tmp); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		if err := quarantineIn(s.c.Base, dst); err != nil {
			return err
		}
	}
	if err := s.made(nil, tmp, os.ModeSticky|0o777); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// sweepSock quarantines every child of dir that is not a sound u<uid>.
func (s *setup) sweepSock(dir string) error {
	names, err := readNames(dir)
	if err != nil {
		return err
	}
	for _, n := range names {
		if !s.sockChild(dir, n) {
			if err := quarantineIn(s.c.Base, filepath.Join(dir, n)); err != nil {
				return err
			}
		}
	}
	return nil
}

// sockChild: dir/n is u<uid> (canonical decimal), a real directory owned by
// that uid, mode 0700.
func (s *setup) sockChild(dir, n string) bool {
	uid, err := strconv.ParseUint(strings.TrimPrefix(n, "u"), 10, 32)
	if err != nil || n != "u"+strconv.FormatUint(uid, 10) {
		return false
	}
	p := filepath.Join(dir, n)
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	st, err := s.l.fstat(fd, p)
	return err == nil && uint64(st.Uid) == uid && uint32(st.Mode)&unix.S_IFMT == unix.S_IFDIR && uint32(st.Mode)&0o7777 == 0o700
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
	case !s.isExact(filepath.Join(s.c.Base, sockName), unix.S_IFDIR, 0o1777):
		return notInstalled("%s is not root:wheel 1777", filepath.Join(s.c.Base, sockName))
	}
	names, err := readNames(filepath.Join(s.c.Base, sockName))
	for _, n := range names {
		if err == nil && !s.sockChild(filepath.Join(s.c.Base, sockName), n) {
			err = notInstalled("%s holds %s, which is not a login's own 0700 directory", filepath.Join(s.c.Base, sockName), n)
		}
	}
	return err
}
