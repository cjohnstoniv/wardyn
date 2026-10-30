// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

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

// SetAuxLockBackoff shortens the first boot's wait between aux lock retries.
func SetAuxLockBackoff(d time.Duration) (restore func()) {
	old := auxLockBackoff
	auxLockBackoff = d
	return func() { auxLockBackoff = old }
}

// SetToolsDialAsks makes stage 2's tools-dial read ask n times, every every. restore undoes it.
func SetToolsDialAsks(n int, every time.Duration) (restore func()) {
	oldN, oldEvery := toolsDialAsks, toolsDialEvery
	toolsDialAsks, toolsDialEvery = n, every
	return func() { toolsDialAsks, toolsDialEvery = oldN, oldEvery }
}

// PairOn serves a boot's pairer for port on l, keyed by key, as a run or stage-2 boot does, until the test
// ends. take hands out the paired guest connection; connected counts the connections that paired.
func PairOn(t testing.TB, l net.Listener, port uint32, key []byte) (take func(context.Context) (net.Conn, error), connected *atomic.Int32) {
	p := newPairer(port, key, nil)
	connected = new(atomic.Int32)
	p.connected = func() { connected.Add(1) }
	go p.acceptGuest(l)
	t.Cleanup(func() { _ = l.Close(); p.close() })
	return p.take, connected
}

// SetPairDeadline makes a guest connection's proof due within d. restore undoes it.
func SetPairDeadline(d time.Duration) (restore func()) {
	old := pairDeadline
	pairDeadline = d
	return func() { pairDeadline = old }
}

// PairPendingMax is how many unproven guest connections a port holds at once.
const PairPendingMax = pairPendingMax

// ReadPairKey is the one check every boot makes of a base's (or a clone's) pairing key in dir.
func ReadPairKey(dir string) ([]byte, error) { return readPairKey(dir) }
