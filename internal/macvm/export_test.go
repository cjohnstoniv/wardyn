// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import "context"

// SetSwapHook makes the base exchange call hook, under the exclusive base
// lock, just before it swaps. restore undoes it.
func SetSwapHook(hook func()) (restore func()) {
	old := swapFn
	swapFn = func(a, b string) error { hook(); return old(a, b) }
	return func() { swapFn = old }
}

// LockBase holds base's lock as a clone (shared) or a swap (exclusive) does.
func LockBase(baseDir string, exclusive bool) (unlock func(), err error) {
	return lockFile(context.Background(), baseLockPath(baseDir), exclusive)
}

// BaseLockFree and UpdateLockFree say whether base's lock, or its update
// lock, could be taken in that mode right now.
func BaseLockFree(baseDir string, exclusive bool) bool {
	return lockFree(baseLockPath(baseDir), exclusive)
}

func UpdateLockFree(baseDir string, exclusive bool) bool {
	return lockFree(updateLockPath(baseDir), exclusive)
}

func lockFree(path string, exclusive bool) bool {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // one non-blocking attempt
	unlock, err := lockFile(ctx, path, exclusive)
	if err != nil {
		return false
	}
	unlock()
	return true
}

// RunExtraPorts makes o's run boot also register ports (tests of the
// listener check).
func RunExtraPorts(o *RunOptions, ports ...uint32) { o.extraPorts = ports }

// SetGeteuid makes prepare see euid as its effective user id. restore undoes it.
func SetGeteuid(euid int) (restore func()) {
	old := geteuid
	geteuid = func() int { return euid }
	return func() { geteuid = old }
}
