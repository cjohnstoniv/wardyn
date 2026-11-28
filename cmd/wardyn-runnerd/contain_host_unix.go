// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// checkStateOwner refuses a runner state directory another user owns: serve, which created it, could not read a
// request this user writes there.
func checkStateOwner(offlineDir string) error {
	st, err := os.Stat(offlineDir)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no runner state in %s: serve has never run with this state directory", offlineDir)
	}
	if err != nil {
		return err
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Geteuid() {
		return fmt.Errorf("the runner state in %s belongs to uid %d; run contain as that user (serve could not read a request this user writes)", offlineDir, sys.Uid)
	}
	return nil
}

// probeServe reports whether a serve holds the lifecycle lock: a shared lock that cannot be taken means one does. A
// lock taken is released at once; serve's own start retries its exclusive lock past this moment (offline.New).
func probeServe(offlineDir string) (bool, error) {
	root, err := os.OpenRoot(offlineDir)
	if err != nil {
		return false, err
	}
	defer root.Close()
	f, err := root.OpenFile("daemon.lock", os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	switch err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); {
	case err == nil:
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return false, nil
	case errors.Is(err, unix.EWOULDBLOCK):
		return true, nil
	default:
		return false, err
	}
}
