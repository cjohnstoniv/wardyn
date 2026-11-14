// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package macvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// lockFile takes a flock on path, creating it 0600. The file must be this
// user's and not a symlink: a slot lock in a directory someone else can write
// would let them stall the helper. It waits until ctx ends.
func lockFile(ctx context.Context, path string, exclusive bool) (unlock func(), err error) {
	_, unlock, err = lockFileHeld(ctx, path, exclusive)
	return unlock, err
}

// lockFileHeld is lockFile that also returns the open lock file, so a caller can tell whether the
// path still names it (a lock file that is removed leaves its holders on an unlinked inode).
func lockFileHeld(ctx context.Context, path string, exclusive bool) (f *os.File, unlock func(), err error) {
	f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("macvm: slot lock: %w", err)
	}
	fi, err := f.Stat()
	if err == nil {
		err = checkOwner(fi)
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("macvm: slot lock %s: %w", path, err)
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	for {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		if err == nil {
			return f, func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, nil, fmt.Errorf("macvm: slot lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// checkOwner refuses a file this user does not own. os gives a file's owner as *syscall.Stat_t; the Stat_t
// of golang.org/x/sys/unix is another type, and asking for it passes every file. A file whose owner
// cannot be read is refused too.
func checkOwner(fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("its owner cannot be read")
	}
	if int(st.Uid) != os.Getuid() {
		return errors.New("not owned by this user")
	}
	return nil
}
