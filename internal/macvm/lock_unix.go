// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package macvm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// lockFile takes a flock on path, creating it 0600. The file must be this
// user's and not a symlink: a slot lock in a directory someone else can write
// would let them stall the helper. It waits until ctx ends.
func lockFile(ctx context.Context, path string, exclusive bool) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("macvm: slot lock: %w", err)
	}
	fi, err := f.Stat()
	if err == nil {
		err = checkOwner(fi)
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("macvm: slot lock %s: %w", path, err)
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	for {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("macvm: slot lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func checkOwner(fi os.FileInfo) error {
	if st, ok := fi.Sys().(*unix.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return errors.New("not owned by this user")
	}
	return nil
}
