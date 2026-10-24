// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// notInstalled is a refusal of the ledger's layout, naming the fix.
func notInstalled(format string, a ...any) error {
	return fmt.Errorf("admission: %s; reinstall the Wardyn runner package, whose installer creates it (a runner never does)", fmt.Sprintf(format, a...))
}

// lockPath is admission.lock in the entries directory's parent.
func (l *Ledger) lockPath() string { return filepath.Join(filepath.Dir(l.c.Dir), lockFile) }

func (l *Ledger) fstat(fd int, name string) (unix.Stat_t, error) {
	if l.c.Fstat != nil {
		return l.c.Fstat(fd, name)
	}
	var st unix.Stat_t
	err := unix.Fstat(fd, &st)
	return st, err
}

const groupOtherWrite = 0o022

// walkDir opens dir from / one component at a time, never following a
// symlink, and fstats each: every directory must be root-owned and either
// not group- or other-writable or sticky (so no other user can replace its
// root-owned child). It returns dir's descriptor.
func (l *Ledger) walkDir(dir string) (int, error) {
	if !filepath.IsAbs(dir) {
		return -1, fmt.Errorf("admission: %s is not an absolute path", dir)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	cur := "/"
	for _, name := range append([]string{""}, strings.Split(strings.TrimPrefix(dir, "/"), "/")...) {
		if name != "" {
			nfd, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			_ = unix.Close(fd)
			cur = filepath.Join(cur, name)
			if err != nil {
				return -1, notInstalled("%s is not a directory this runner can open (%v)", cur, err)
			}
			fd = nfd
		}
		st, err := l.fstat(fd, cur)
		if err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
		mode := uint32(st.Mode)
		switch {
		case st.Uid != 0:
			err = notInstalled("%s is not owned by root", cur)
		case mode&unix.S_IFMT != unix.S_IFDIR:
			err = notInstalled("%s is not a directory", cur)
		case mode&groupOtherWrite != 0 && mode&unix.S_ISVTX == 0:
			err = notInstalled("%s is writable by other users and not sticky", cur)
		}
		if err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}

// openLock opens admission.lock through walkDir and checks it and its
// directory by fstat: the directory not group- or other-writable at all, the
// lock a root-owned regular file that is not group- or other-writable.
func (l *Ledger) openLock() (*os.File, error) {
	path := l.lockPath()
	dfd, err := l.walkDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer unix.Close(dfd)
	if st, err := l.fstat(dfd, filepath.Dir(path)); err != nil {
		return nil, err
	} else if uint32(st.Mode)&groupOtherWrite != 0 {
		return nil, notInstalled("%s, which holds the lock, is writable by other users", filepath.Dir(path))
	}
	fd, err := unix.Openat(dfd, lockFile, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, notInstalled("%s cannot be opened (%v)", path, err)
	}
	st, err := l.fstat(fd, path)
	if err == nil {
		mode := uint32(st.Mode)
		switch {
		case st.Uid != 0:
			err = notInstalled("%s is not owned by root", path)
		case mode&unix.S_IFMT != unix.S_IFREG:
			err = notInstalled("%s is not a regular file", path)
		case mode&groupOtherWrite != 0:
			err = notInstalled("%s is writable by other users", path)
		}
	}
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// checkEntriesDir checks the entries directory by fstat: root-owned and
// sticky, so a login can remove only its own entries.
func (l *Ledger) checkEntriesDir() error {
	pfd, err := l.walkDir(filepath.Dir(l.c.Dir))
	if err != nil {
		return err
	}
	defer unix.Close(pfd)
	fd, err := unix.Openat(pfd, filepath.Base(l.c.Dir), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return notInstalled("the macOS VM ledger %s is missing (%v)", l.c.Dir, err)
	}
	defer unix.Close(fd)
	st, err := l.fstat(fd, l.c.Dir)
	if err != nil {
		return err
	}
	if st.Uid != 0 || uint32(st.Mode)&unix.S_ISVTX == 0 {
		return notInstalled("%s must be a root-owned sticky directory (mode 1777)", l.c.Dir)
	}
	return nil
}

func (l *Ledger) withLock(fn func() error) error {
	f, err := l.openLock()
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(l.c.LockWait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		if time.Now().After(deadline) {
			return ErrBusy
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	if l.c.afterFlock != nil {
		l.c.afterFlock()
	}
	// The path must still name the file locked: a rename or unlink between
	// the open and the flock would leave two runners on two locks.
	var held, now unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &held); err != nil {
		return err
	}
	if err := unix.Lstat(l.lockPath(), &now); err != nil || now.Dev != held.Dev || now.Ino != held.Ino {
		return notInstalled("%s changed while this runner locked it", l.lockPath())
	}
	return fn()
}
