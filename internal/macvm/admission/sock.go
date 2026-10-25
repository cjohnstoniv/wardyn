// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// DefaultSockDir is where every login's macOS VM sockets live, outside the
// home so their paths stay short (a macOS sockaddr_un path is at most 103
// bytes). The runner package's postinstall makes it root:wheel 1777 (sticky),
// beside the ledger:
//
//	/Users/Shared/Wardyn/sock/                     root:wheel 1777 (postinstall)
//	/Users/Shared/Wardyn/sock/u<uid>/              the login's, mode 0700 (one mkdir(2))
//	/Users/Shared/Wardyn/sock/u<uid>/<run-key>/    the run's, mode 0700 (one mkdir(2))
//	    ctl.sock agent.sock desktop.sock
//
// The sticky bit means only the login that made u<uid> (or root) can rename
// or remove it, so once a descriptor walk has checked it, a path-based bind
// or connect through it reaches the same directory. A name another login
// planted there (a file, a link, a directory with another owner or mode) is
// refused, never used or fixed: a denial of that login's macOS VMs, never
// access to them.
const DefaultSockDir = "/Users/Shared/Wardyn/sock"

// SockConfig locates one login's socket directories.
type SockConfig struct {
	Dir string // "" is DefaultSockDir; tests use their own root
	UID int    // the login's uid (os.Getuid())
	// Fstat, if set, replaces fstat(2) in the checks (tests), as Config.Fstat.
	Fstat func(fd int, name string) (unix.Stat_t, error)
}

func (c SockConfig) dir() string {
	if c.Dir == "" {
		return DefaultSockDir
	}
	return filepath.Clean(c.Dir)
}

// UserDir is the login's own directory, <dir>/u<uid>.
func (c SockConfig) UserDir() string { return filepath.Join(c.dir(), "u"+strconv.Itoa(c.UID)) }

// SockRefusal is a socket directory that is not exactly what this login
// makes. Nothing is changed in it.
type SockRefusal struct{ Path, Why string }

func (r *SockRefusal) Error() string {
	return fmt.Sprintf("admission: %s is not this login's macOS VM socket directory (%s); until it is removed, this login's macOS VMs cannot start", r.Path, r.Why)
}

// openUser opens u<uid> through the ledger's descriptor walk: every
// component of the sock dir root-owned (sticky where writable), the sock dir
// itself root-owned and sticky, and u<uid> a directory, not a link, owned by
// the login, mode exactly 0700. A missing u<uid> is made with one mkdir(2).
func (c SockConfig) openUser() (int, error) {
	l := &Ledger{c: Config{Fstat: c.Fstat}}
	pfd, err := l.walkDir(c.dir())
	if err != nil {
		return -1, err
	}
	defer unix.Close(pfd)
	st, err := l.fstat(pfd, c.dir())
	if err != nil {
		return -1, err
	}
	if uint32(st.Mode)&unix.S_ISVTX == 0 {
		return -1, notInstalled("%s must be a root-owned sticky directory (mode 1777)", c.dir())
	}
	return c.openOwn(l, pfd, filepath.Base(c.UserDir()), c.UserDir(), false)
}

// openOwn opens name in the directory pfd as one of this login's 0700
// directories, making it first with one mkdir(2) when mustCreate is set (an
// existing one is then refused) or when it is missing. A directory this call
// made gets fchmod 0700 once its owner is checked, because mkdir's mode
// passes through the umask; one found is never changed, only checked.
func (c SockConfig) openOwn(l *Ledger, pfd int, name, path string, mustCreate bool) (int, error) {
	made := false
	err := unix.Mkdirat(pfd, name, 0o700)
	switch {
	case err == nil:
		made = true
	case errors.Is(err, unix.EEXIST) && mustCreate:
		return -1, fmt.Errorf("admission: a macOS VM socket directory is already at %s", path)
	case !errors.Is(err, unix.EEXIST):
		return -1, err
	}
	fd, err := unix.Openat(pfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &SockRefusal{Path: path, Why: "not a directory this login can open: " + err.Error()}
	}
	st, err := l.fstat(fd, path)
	if err == nil && made && int(st.Uid) == c.UID && uint32(st.Mode)&unix.S_IFMT == unix.S_IFDIR {
		if err = unix.Fchmod(fd, 0o700); err == nil {
			st, err = l.fstat(fd, path)
		}
	}
	if err == nil {
		err = checkOwn(st, path, c.UID)
	}
	if err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func checkOwn(st unix.Stat_t, path string, uid int) error {
	mode := uint32(st.Mode)
	switch {
	case mode&unix.S_IFMT != unix.S_IFDIR:
		return &SockRefusal{Path: path, Why: "not a directory"}
	case int(st.Uid) != uid:
		return &SockRefusal{Path: path, Why: fmt.Sprintf("owned by uid %d, not %d", st.Uid, uid)}
	case mode&0o7777 != 0o700:
		return &SockRefusal{Path: path, Why: fmt.Sprintf("mode %04o, not 0700", mode&0o7777)}
	}
	return nil
}

// MakeRunDir makes a run's socket directory, <dir>/u<uid>/<key>, new, after
// checking u<uid>: one mkdir(2), and a directory already there is refused,
// never reused (the run directory's rule).
func (c SockConfig) MakeRunDir(key string) (string, error) {
	return c.runDir(key, true)
}

// CheckRunDir checks an existing run socket directory the same way, for
// every later open (a dial, a status).
func (c SockConfig) CheckRunDir(key string) (string, error) {
	return c.runDir(key, false)
}

func (c SockConfig) runDir(key string, create bool) (string, error) {
	if key == "" || key != filepath.Base(key) || key == "." || key == ".." {
		return "", fmt.Errorf("admission: %q is not a run key", key)
	}
	ufd, err := c.openUser()
	if err != nil {
		return "", err
	}
	defer unix.Close(ufd)
	path := filepath.Join(c.UserDir(), key)
	l := &Ledger{c: Config{Fstat: c.Fstat}}
	if !create {
		fd, err := unix.Openat(ufd, key, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return "", &SockRefusal{Path: path, Why: "not a directory this login can open: " + err.Error()}
		}
		defer unix.Close(fd)
		st, err := l.fstat(fd, path)
		if err != nil {
			return "", err
		}
		return path, checkOwn(st, path, c.UID)
	}
	fd, err := c.openOwn(l, ufd, key, path, true)
	if err != nil {
		return "", err
	}
	_ = unix.Close(fd)
	return path, nil
}

// SweepRunDirs removes the run socket directories under u<uid> whose run is
// gone (gone(key) true): what a crash left after its run directory went. It
// checks u<uid> first and touches nothing else; it returns the keys removed.
// The helper runs it at each `run` start; runnerd runs it wherever it sweeps
// a dead run directory.
func (c SockConfig) SweepRunDirs(gone func(key string) bool) ([]string, error) {
	ufd, err := c.openUser()
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(ufd), c.UserDir())
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	var removed []string
	var errs []error
	for _, n := range names {
		if !gone(n) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(c.UserDir(), n)); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, n)
	}
	return removed, errors.Join(errs...)
}
