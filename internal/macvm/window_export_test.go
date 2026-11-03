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

// HasNativeWindowScreen says whether this build has the Mac's own window screen.
func HasNativeWindowScreen() bool { return nativeWindowScreen != nil }

// WatchWindowClosed is watchWindowClosed.
func WatchWindowClosed(up func() int, every time.Duration, closed chan<- struct{}) {
	watchWindowClosed(up, every, closed)
}
