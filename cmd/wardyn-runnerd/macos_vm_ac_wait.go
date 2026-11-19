// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// toolsUpdateWaitingLine is what `macos-vm status` and doctor say while a tools update waits for AC power.
const toolsUpdateWaitingLine = "tools update waiting for AC power"

// toolsUpdateWaiting reports whether a tools update of the base under root is paused on battery: the last
// progress event in tools-update.log is the tools phase paused (the helper logs it when the Mac goes on
// battery and again, unpaused, when AC returns), and the base's update lock is still held, so a helper that
// died while paused does not leave the line up.
func toolsUpdateWaiting(root string) bool {
	ev, ok, _, _ := lastHelperLines(filepath.Join(root, toolsUpdateLog))
	return ok && ev.Phase == macvm.PhaseTools && ev.Paused && updateLockHeld(macvm.Layout{Root: root}.BaseDir(0))
}

// updateLockHeld reports whether another process holds base's update lock (macvm updateLockPath). It takes
// no lock itself and creates nothing.
func updateLockHeld(base string) bool {
	f, err := os.OpenFile(base+updateLockSuffix, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return err == unix.EWOULDBLOCK
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false
}

// toolsUpdateWaitWarnings is doctor's line for a tools update waiting for AC power: none when it is not.
func toolsUpdateWaitWarnings(root string) []string {
	if toolsUpdateWaiting(root) {
		return []string{toolsUpdateWaitingLine}
	}
	return nil
}
