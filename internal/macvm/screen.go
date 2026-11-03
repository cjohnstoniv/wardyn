// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"errors"
	"time"
	"unsafe"
)

// ErrNoDesktopSession is why a prepare that needs setup by hand cannot open the guest's window: the
// process is not in the Mac's desktop session (started over SSH, or outside the signed-in login), or runs
// as root. Its text is what the person reads.
var ErrNoDesktopSession = errors.New("setup by hand needs this Mac's desktop: run prepare in Terminal on the Mac, signed in at its desktop, not over SSH and not with sudo")

// NoDesktopFailurePrefix starts the error a setup by hand ends with when this process cannot reach the Mac's
// desktop; the cause follows it.
const NoDesktopFailurePrefix = "macvm: no desktop: "

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

// WindowStrip is the line under a run window's title bar.
const WindowStrip = "What you type goes into this run. Sign-ins stay on the run's disk until it ends. Close this window to hand back; the run keeps going."

// WindowText is a run window's text: its title and the strip under the title bar.
type WindowText struct{ Title, Strip string }

// ErrNotAtMac is what WindowScreen.Ready wraps when the Mac's screen is locked or its console session is not
// this process's. Any other Ready error means the check could not be read; both refuse a window.
var ErrNotAtMac = errors.New("macvm: this Mac's screen is locked or its console session is not this user's")

// WindowScreen draws a run's window on the Mac. The darwin cgo build supplies one (nativeWindowScreen); tests
// supply a fake through RunOptions.Window.
type WindowScreen interface {
	// Ready is nil when a window can open now: the screen is unlocked and this process's session is the Mac's
	// console. It is asked before a window opens and every 2 s while one is up.
	Ready() error
	// Open puts a close-only window on vm (a VZVirtualMachine the caller keeps alive until the window ends).
	Open(vm unsafe.Pointer, text WindowText) (Window, error)
}

// Window is one open run window.
type Window interface {
	// Closed is closed when the person closes the window (or Close has run).
	Closed() <-chan struct{}
	// Close detaches the view and closes the window; it never stops the VM. It returns once the window is
	// gone, and is safe to call twice.
	Close()
}

// ViewMachine is a Machine that can give the pointer a window's view draws: the engine's VZVirtualMachine.
// Fakes and the other engines need not implement it; a run on a machine without it refuses a window.
type ViewMachine interface {
	// ViewHandle returns the VZVirtualMachine pointer, or why it cannot be read (the engine's layout check
	// failed). The pointer is valid while the Machine is.
	ViewHandle() (unsafe.Pointer, error)
}

// nativeWindowScreen is the Mac's WindowScreen; set by the darwin cgo build (screen_darwin.go).
var nativeWindowScreen WindowScreen

// watchWindowClosed closes closed once up() is no longer positive: the window's state goes to 0 when the person
// closes it (or Close ran), and to -1 if it could not be made. It polls every interval.
func watchWindowClosed(up func() int, interval time.Duration, closed chan<- struct{}) {
	go func() {
		for up() > 0 {
			time.Sleep(interval)
		}
		close(closed)
	}()
}
