// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package admission

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	// Logf reports a directory skipped or an entry quarantined; nil: standard error.
	Logf func(format string, a ...any)

	afterFlock func()                           // tests: runs once the old lock's flock is held
	beforeMove func(dst string) error           // tests: runs before each entry is moved
	afterSock  func()                           // tests: runs once sock/ is swept
	afterSwap  func(old string)                 // tests: runs once relink swapped the new entries directory in; old is where the old one now is
	fault      func(op, dir, name string) error // tests: an error here replaces the step op ("stat", "readdir", "quarantine") on dir's name
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
// is not exact is moved into a quarantine beside it and made again, and a
// child that is not exact is replaced. Every entry it can find in a staging
// directory or quarantine an earlier run left moves into an exact entries
// directory, which is never emptied, or with the entries directory's own into
// a new one, or it refuses: the entries directory never ends with fewer
// entries than it held. One a helper accepts is never emptied either, even by
// a run that fails or dies: its replacement is built from second links. It touches nothing outside Base and its
// .wardyn-quarantine.* siblings.
func Setup(c SetupConfig) error {
	if c.LockWait == 0 {
		c.LockWait = LockWait
	}
	if c.Chown == nil {
		c.Chown = func(f *os.File) error { return f.Chown(0, 0) }
	}
	if c.Logf == nil {
		c.Logf = func(format string, a ...any) { fmt.Fprintf(os.Stderr, "admission: "+format+"\n", a...) }
	}
	s := &setup{c: c, l: &Ledger{c: Config{Dir: filepath.Join(c.Base, entriesName), Fstat: c.Fstat, LockWait: c.LockWait}}}
	defer s.closeAll()
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
	if s.c.afterSock != nil {
		s.c.afterSock()
	}
	if !keepLock {
		if err := s.newLock(); err != nil {
			return err
		}
	}
	return s.verify()
}

type setup struct {
	c     SetupConfig
	l     *Ledger
	id    [2]uint64  // the base's dev and inode
	base  *os.File   // the base, opened again for the entries
	files []*os.File // every directory opened for the entries
}

func (s *setup) closeAll() {
	for _, f := range s.files {
		f.Close()
	}
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
	if err != nil {
		f.Close()
		return nil, false, err
	}
	if uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG {
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

// source is a directory that may hold entries, opened from its parent's
// descriptor without following a link.
type source struct {
	f      *os.File
	parent *os.File // nil: Base's entries directory, never removed
	name   string   // its name in parent
	names  []string // its entries
	st     unix.Stat_t
}

// entries makes Base's entries directory exact. An exact one is never
// drained: every other source's entries merge into it. One a helper accepts
// (root-owned and sticky) is never drained either: the sources merge into it
// and relink replaces it. Otherwise (no helper counts it) every entry found
// moves into a staging directory that stays root-only 0700 until all are in
// it and counted, which then becomes 1777 and takes the final name in one
// rename. A source left empty is removed.
func (s *setup) entries() error {
	dst, srcs, err := s.entrySources()
	if err != nil {
		return err
	}
	switch {
	case dst != nil && exact(dst.st, unix.S_IFDIR, 0o1777):
		err = s.merge(dst, srcs)
	case dst != nil && dst.st.Uid == 0 && uint32(dst.st.Mode)&unix.S_ISVTX != 0: // checkEntriesDir's rule
		if err = s.merge(dst, srcs); err == nil {
			err = s.relink(dst)
		}
	default:
		err = s.stage(dst, srcs)
	}
	if err != nil {
		return err
	}
	for _, src := range srcs {
		_ = unix.Unlinkat(int(src.parent.Fd()), src.name, unix.AT_REMOVEDIR) // rmdir: only an empty one
	}
	return nil
}

// merge moves each source's entries into dst, an exact entries directory, one
// renameat each, never over a name: dst only ever gains. A copy whose name dst
// holds already goes to a root-only quarantine (dst's copy counts), and one
// gone when its move comes (its owner removed it) is skipped. Any other error
// stops the merge with dst holding at least what it held.
func (s *setup) merge(dst *source, srcs []*source) error {
	for _, src := range srcs {
		for _, n := range src.names {
			err := s.move(src, n, dst.f)
			if errors.Is(err, unix.EEXIST) {
				err = s.quarantineEntry(src, n, dst.f.Name())
			}
			if err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
	}
	return nil
}

// quarantineEntry moves src's entry n into a new root-only directory in Base.
func (s *setup) quarantineEntry(src *source, n, dst string) error {
	return s.quarantineName(src.f, n, fmt.Sprintf("%s holds %s already; the copy in %s", dst, n, src.f.Name()))
}

// quarantineName moves from's entry n into a new root-only directory in Base, and logs why.
func (s *setup) quarantineName(from *os.File, n, why string) error {
	q, err := os.MkdirTemp(s.c.Base, ".wardyn-quarantine.")
	if err != nil {
		return err
	}
	qf, _, err := s.openDir(int(s.base.Fd()), filepath.Base(q), q)
	if err != nil {
		return err
	}
	defer qf.Close()
	s.c.Logf("%s moves to %s", why, q)
	if err = s.faulted("quarantine", from.Name(), n); err == nil {
		err = renameNoReplace(int(from.Fd()), n, int(qf.Fd()), n)
	}
	if errors.Is(err, unix.ENOENT) { // its owner removed it: no empty quarantine behind
		_ = unix.Unlinkat(int(s.base.Fd()), filepath.Base(q), unix.AT_REMOVEDIR)
	}
	return err
}

// faulted is the error a test plants in step op on dir's name, else nil.
func (s *setup) faulted(op, dir, name string) error {
	if s.c.fault == nil {
		return nil
	}
	return s.c.fault(op, dir, name)
}

// statName is the lstat of dir's entry n.
func (s *setup) statName(dir *os.File, n string) (st unix.Stat_t, err error) {
	if err = s.faulted("stat", dir.Name(), n); err != nil {
		return st, err
	}
	return st, unix.Fstatat(int(dir.Fd()), n, &st, unix.AT_SYMLINK_NOFOLLOW)
}

// linkPrefix names the directory relink builds a replacement in, and where
// the replaced directory lands. It only ever holds second links to entries
// the entries directory holds, or held until their owners released them, so
// every run discards it and none carries it forward. Earlier installers do
// not read it: it does not start with "."+entriesName+".".
const linkPrefix = "." + entriesName + "-link."

// relink replaces dst, an entries directory a helper accepts that is not
// exact, without taking an entry out of it: each is hard-linked into a new
// root-only directory, which, once all are in it and counted, becomes 1777
// and swaps with dst in one rename. Until the swap every helper, of any
// release, counts dst whole; after it the same files are in the new one. A
// run that fails or dies first leaves dst as it was. The old directory, now
// under linkPrefix, is discarded.
func (s *setup) relink(dst *source) error {
	tmp, err := os.MkdirTemp(s.c.Base, linkPrefix)
	if err != nil {
		return err
	}
	tf, _, err := s.openDir(int(s.base.Fd()), filepath.Base(tmp), tmp)
	if err != nil {
		return err
	}
	s.files = append(s.files, tf)
	if _, err := dst.f.Seek(0, io.SeekStart); err != nil { // the merge added names
		return err
	}
	names, err := dst.f.Readdirnames(-1)
	if err != nil {
		return err
	}
	linked := 0
	for _, n := range names {
		if s.c.beforeMove != nil {
			if err := s.c.beforeMove(filepath.Join(tmp, n)); err != nil {
				return err
			}
		}
		switch err := unix.Linkat(int(dst.f.Fd()), n, int(tf.Fd()), n, 0); {
		case err == nil:
			linked++
		case errors.Is(err, unix.ENOENT): // its owner removed it
		case errors.Is(err, unix.EPERM) && notRegular(dst.f, n):
			// Not a file a helper reads as a VM's entry, and a hard link to it fails: any login can make
			// one in this world-writable directory, and it must not stop every setup. It goes to a
			// root-only quarantine. A regular file that fails the same way (the immutable flag) cannot be
			// renamed either, so that one still stops the run.
			if err := s.quarantineName(dst.f, n, fmt.Sprintf("%s holds %s, which is not a file and cannot be linked", dst.f.Name(), n)); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		default:
			return fmt.Errorf("admission: linking %s into %s: %w", filepath.Join(dst.f.Name(), n), tmp, err)
		}
	}
	if got, err := readNames(tmp); err != nil || len(got) != linked {
		return notInstalled("linking the macOS VM entries found %d of %d (%v)", len(got), linked, err)
	}
	if err := s.made(nil, tmp, os.ModeSticky|0o777); err != nil {
		return err
	}
	if err := renameSwap(int(s.base.Fd()), filepath.Base(tmp), entriesName); err != nil {
		return err
	}
	if s.c.afterSwap != nil {
		s.c.afterSwap(tmp)
	}
	if s.keepUnpublished(filepath.Base(tmp)) {
		s.discard(filepath.Base(tmp))
	}
	return nil
}

// notRegular reports whether dir's entry n is there and is not a regular file.
func notRegular(dir *os.File, n string) bool {
	var st unix.Stat_t
	return unix.Fstatat(int(dir.Fd()), n, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG
}

// discard empties and removes name, a linkPrefix directory in Base.
func (s *setup) discard(name string) {
	f, _, err := s.rootDir(s.base, name)
	if f == nil {
		if err != nil {
			s.c.Logf("left %s: %v", filepath.Join(s.base.Name(), name), err)
		}
		return
	}
	names, err := dirNames(f)
	if err != nil {
		s.c.Logf("left some of %s: %v", f.Name(), err)
	}
	for _, n := range names {
		if err := unix.Unlinkat(int(f.Fd()), n, 0); err != nil {
			s.c.Logf("left %s: %v", filepath.Join(f.Name(), n), err)
		}
	}
	_ = unix.Unlinkat(int(s.base.Fd()), name, unix.AT_REMOVEDIR)
}

// keepUnpublished moves each name of name, the entries directory relink just swapped out, to a root-only
// quarantine unless its inode is also in the published entries directory, so the discard that follows
// unlinks only second links. Under the setup's lock every name is in both, and none moves. It reports
// whether the discard may go on: not after a name it could not move.
func (s *setup) keepUnpublished(name string) bool {
	f, _, err := s.rootDir(s.base, name)
	if f == nil {
		if err != nil { // a second open in discard that works would skip the proof
			s.c.Logf("left %s: %v", filepath.Join(s.base.Name(), name), err)
		}
		return err == nil // skipped or gone: discard says why
	}
	held, err := s.publishedInodes()
	names, nerr := dirNames(f)
	if err != nil || nerr != nil {
		s.c.Logf("left %s: %v", f.Name(), errors.Join(err, nerr))
		return false
	}
	ok := true
	for _, n := range names {
		st, err := s.statName(f, n)
		if errors.Is(err, unix.ENOENT) {
			continue // its owner removed it
		}
		if err != nil { // no inode to compare: keep the name, and so the directory
			s.c.Logf("left %s: %v", filepath.Join(f.Name(), n), err)
			ok = false
			continue
		}
		if held[[2]uint64{uint64(st.Dev), st.Ino}] {
			continue
		}
		why := fmt.Sprintf("%s holds %s, which the entries directory does not", f.Name(), n)
		if err := s.quarantineName(f, n, why); err != nil && !errors.Is(err, unix.ENOENT) {
			s.c.Logf("left %s: %v", filepath.Join(f.Name(), n), err)
			ok = false
		}
	}
	return ok
}

// publishedInodes is the device and inode of each name in Base's entries directory.
func (s *setup) publishedInodes() (map[[2]uint64]bool, error) {
	f, _, err := s.openDir(int(s.base.Fd()), entriesName, filepath.Join(s.base.Name(), entriesName))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err == nil {
		err = s.faulted("readdir", f.Name(), "")
	}
	if err != nil {
		return nil, err
	}
	held := make(map[[2]uint64]bool, len(names))
	for _, n := range names {
		// A name that cannot be statted stays out of held, so its twin in the old directory is quarantined.
		if st, err := s.statName(f, n); err == nil {
			held[[2]uint64{uint64(st.Dev), st.Ino}] = true
		}
	}
	return held, nil
}

// stage moves dst's entries (dst is nil when there is no entries directory to
// open) and every source's into a new root-only directory, counts them, and
// gives it dst's name.
func (s *setup) stage(dst *source, srcs []*source) error {
	path := filepath.Join(s.c.Base, entriesName)
	tmp, err := os.MkdirTemp(s.c.Base, "."+entriesName+".")
	if err != nil {
		return err
	}
	tf, _, err := s.openDir(int(s.base.Fd()), filepath.Base(tmp), tmp)
	if err != nil {
		return err
	}
	s.files = append(s.files, tf)
	if dst != nil {
		srcs = append([]*source{dst}, srcs...)
	}
	moved := 0
	for _, src := range srcs {
		for _, n := range src.names {
			switch err := s.move(src, n, tf); {
			case err == nil:
				moved++
			case errors.Is(err, unix.ENOENT): // its owner removed it
			case errors.Is(err, unix.EEXIST):
				return notInstalled("two macOS VM entries are named %s (%s)", n, src.f.Name())
			default:
				return err
			}
		}
	}
	if got, err := readNames(tmp); err != nil || len(got) != moved {
		return notInstalled("moving the macOS VM entries found %d of %d (%v)", len(got), moved, err)
	}
	if used, err := occupied(path); err != nil {
		return err
	} else if used {
		if err := quarantineIn(s.c.Base, path); err != nil {
			return err
		}
	}
	if err := s.made(nil, tmp, os.ModeSticky|0o777); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// move renames src's entry n into to, never over a name there (EEXIST).
func (s *setup) move(src *source, n string, to *os.File) error {
	if s.c.beforeMove != nil {
		if err := s.c.beforeMove(filepath.Join(to.Name(), n)); err != nil {
			return err
		}
	}
	return renameNoReplace(int(src.f.Fd()), n, int(to.Fd()), n)
}

// entrySources opens Base's entries directory (nil if what is there is not a
// directory, a link, or nothing; any other failure to open or fstat it is an
// error) and every other directory that may hold entries: staging
// directories a failed run left, and the entries directories in earlier
// quarantines, in Base and beside it (a rebuilt base's). A linkPrefix
// directory holds none, and is discarded. Each is opened from its parent's
// descriptor one component at a time, never through a link, and every
// component must be a root-owned directory by its fstat; one whose device
// and inode were seen already (Base's entries directory first) is skipped.
func (s *setup) entrySources() (*source, []*source, error) {
	bfd, err := s.l.walkDir(s.c.Base)
	if err != nil {
		return nil, nil, err
	}
	s.base = os.NewFile(uintptr(bfd), s.c.Base)
	s.files = append(s.files, s.base)
	if st, err := s.l.fstat(bfd, s.c.Base); err != nil || [2]uint64{uint64(st.Dev), st.Ino} != s.id {
		return nil, nil, notInstalled("%s changed while the installer set it up", s.c.Base)
	}
	pfd, err := s.l.walkDir(filepath.Dir(s.c.Base))
	if err != nil {
		return nil, nil, err
	}
	parent := os.NewFile(uintptr(pfd), filepath.Dir(s.c.Base))
	s.files = append(s.files, parent)

	seen := map[[2]uint64]bool{}
	var dst *source
	path := filepath.Join(s.c.Base, entriesName)
	switch f, st, err := s.openDir(bfd, entriesName, path); {
	case err == nil:
		s.files = append(s.files, f)
		dst = &source{f: f, st: st}
		seen[[2]uint64{uint64(st.Dev), st.Ino}] = true
		if dst.names, err = f.Readdirnames(-1); err != nil {
			return nil, nil, err
		}
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ELOOP):
		// none, or not a directory (a file, a link): stage replaces it
	default: // one that cannot be read may hold entries: never replaced
		return nil, nil, notInstalled("%s cannot be inspected (%v)", path, err)
	}
	var srcs []*source
	var readErr error // the first source that cannot be inspected or read
	keep := func(err error) {
		if err != nil && readErr == nil {
			readErr = err
		}
	}
	add := func(at *os.File, name string) {
		f, id, err := s.rootDir(at, name)
		keep(err)
		switch {
		case f == nil:
		case seen[id]:
			s.c.Logf("skipped %s: the same directory as one already read", f.Name())
		default:
			seen[id] = true
			names, err := f.Readdirnames(-1)
			keep(err)
			srcs = append(srcs, &source{f: f, parent: at, name: name, names: names})
		}
	}
	names, err := dirNames(s.base)
	keep(err)
	for _, n := range names {
		switch {
		case strings.HasPrefix(n, linkPrefix):
			// A directory an earlier run kept after a failed quarantine holds names the discard must not unlink.
			if s.keepUnpublished(n) {
				s.discard(n)
			}
		case strings.HasPrefix(n, "."+entriesName+"."):
			add(s.base, n)
		case strings.HasPrefix(n, quarantinePrefix):
			q, _, err := s.rootDir(s.base, n)
			keep(err)
			if q != nil {
				add(q, entriesName)
			}
		}
	}
	names, err = dirNames(parent)
	keep(err)
	for _, n := range names {
		if !strings.HasPrefix(n, quarantinePrefix) {
			continue
		}
		q, _, err := s.rootDir(parent, n)
		keep(err)
		if q == nil {
			continue
		}
		w, _, err := s.rootDir(q, filepath.Base(s.c.Base))
		keep(err)
		if w != nil {
			add(w, entriesName)
		}
	}
	return dst, srcs, readErr
}

const quarantinePrefix = ".wardyn-quarantine."

// dirNames lists the directory open at f, sorted.
func dirNames(f *os.File) ([]string, error) {
	names, err := f.Readdirnames(-1)
	sort.Strings(names)
	return names, err
}

// rootDir opens name in at, never following a link, and returns it with its
// device and inode if it is a root-owned directory by its fstat. Nothing
// there is skipped quietly; anything else (a link, a login's directory, not
// a directory) is skipped and logged. One that cannot be opened or fstatted
// for any other reason may hold entries, so it is an error, not a skip.
func (s *setup) rootDir(at *os.File, name string) (*os.File, [2]uint64, error) {
	path := filepath.Join(at.Name(), name)
	f, st, err := s.openDir(int(at.Fd()), name, path)
	switch {
	case err == nil && st.Uid != 0:
		f.Close()
		s.c.Logf("skipped %s: not a root-owned directory (not owned by root)", path)
	case err == nil:
		s.files = append(s.files, f)
		return f, [2]uint64{uint64(st.Dev), st.Ino}, nil
	case errors.Is(err, unix.ENOENT):
	case errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ELOOP):
		s.c.Logf("skipped %s: not a root-owned directory (%v)", path, err)
	default:
		return nil, [2]uint64{}, notInstalled("%s cannot be inspected (%v)", path, err)
	}
	return nil, [2]uint64{}, nil
}

// openDir opens name in the directory at without following a link, and
// fstats it: it must be a directory.
func (s *setup) openDir(at int, name, path string) (*os.File, unix.Stat_t, error) {
	fd, err := unix.Openat(at, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, unix.Stat_t{}, err
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := s.l.fstat(fd, path)
	if err == nil && uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR {
		err = unix.ENOTDIR
	}
	if err != nil {
		f.Close()
		return nil, st, err
	}
	return f, st, nil
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

// occupied reports whether anything is at path. An Lstat error other than not-exist is returned, because the
// rename that follows is no refusal: over an empty directory it succeeds and replaces it (nothing is lost, as
// it is empty), so the step stops here instead.
func occupied(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, err
}

// moveAll renames each of names from src into dst, never over something there.
func (s *setup) moveAll(src string, names []string, dst string) error {
	for _, n := range names {
		to := filepath.Join(dst, n)
		if used, err := occupied(to); err != nil {
			return err
		} else if used {
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
	if used, err := occupied(dst); err != nil {
		return err
	} else if used {
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
// inode), and checks the whole layout. sock/'s children are the sweep's to
// judge: sock/ is 1777, so one a login makes after it waits for the next one.
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
	return nil
}
