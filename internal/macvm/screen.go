// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import "errors"

// ErrNoDesktopSession is why a prepare that needs setup by hand cannot open the guest's window: the
// process is not in the Mac's desktop session (started over SSH, or outside the signed-in login), or runs
// as root. Its text is what the person reads.
var ErrNoDesktopSession = errors.New("setup by hand needs this Mac's desktop: run prepare in Terminal on the Mac, signed in at its desktop, not over SSH and not with sudo")

// ServeScreens runs work and returns its exit code. The helper's main goroutine calls it, locked to the
// process's main thread from init, because AppKit runs only there: on darwin that thread waits while work
// runs, so a first boot's Screen can open the guest's window on it. The first window makes the thread
// AppKit's for good, and work's exit code then ends the process (os.Exit). Without the darwin build, or
// off the main thread, it is work().
func ServeScreens(work func() int) int {
	if serveScreens == nil {
		return work()
	}
	return serveScreens(work)
}

// serveScreens is set by the darwin cgo build (screen_darwin.go).
var serveScreens func(work func() int) int
