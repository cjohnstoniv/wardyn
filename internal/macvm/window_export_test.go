// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import "time"

// SetControlOwnerUID makes ctl.sock's owner uid uid, so a test peer is a stranger. restore undoes it.
func SetControlOwnerUID(uid int64) (restore func()) {
	old := controlOwnerUID.Load()
	controlOwnerUID.Store(uid)
	return func() { controlOwnerUID.Store(old) }
}

// SetWindowRecheck makes an open window ask its console check every d. restore undoes it.
func SetWindowRecheck(d time.Duration) (restore func()) {
	old := windowRecheck.Load()
	windowRecheck.Store(int64(d))
	return func() { windowRecheck.Store(old) }
}

// SetWindowReleaseWait makes an open wait at most d for a window whose holder is ending. restore undoes it.
func SetWindowReleaseWait(d time.Duration) (restore func()) {
	old := windowReleaseWait.Load()
	windowReleaseWait.Store(int64(d))
	return func() { windowReleaseWait.Store(old) }
}

// SetWindowReleaseHook runs f once a window's hold has ended, before it is closed and its slot freed. restore
// undoes it.
func SetWindowReleaseHook(f func()) (restore func()) {
	old := windowReleaseHook.Swap(&f)
	return func() { windowReleaseHook.Store(old) }
}

// SetWindowWaitHook runs f each time an open starts waiting for an ending window's slot. restore undoes it.
func SetWindowWaitHook(f func()) (restore func()) {
	old := windowWaitHook.Swap(&f)
	return func() { windowWaitHook.Store(old) }
}

// SetWindowWaitedHook runs f when an open that waited has checked its own connection. restore undoes it.
func SetWindowWaitedHook(f func()) (restore func()) {
	old := windowWaitedHook.Swap(&f)
	return func() { windowWaitedHook.Store(old) }
}

// ErrConsoleUnreadable is errConsoleUnreadable.
var ErrConsoleUnreadable = errConsoleUnreadable

// HasNativeWindowScreen says whether this build has the Mac's own window screen.
func HasNativeWindowScreen() bool { return nativeWindowScreen != nil }

// WatchWindow is watchWindow.
func WatchWindow(up func() int, lockNotices func() int64, since int64, every time.Duration, closed, locked chan<- struct{}) {
	watchWindow(up, lockNotices, since, every, closed, locked)
}
