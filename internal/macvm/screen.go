// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"errors"
	"fmt"
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

// The line under a run window's title bar. The kept-machine words are a draft until its board is approved;
// a copy change is an edit to this block.
const (
	WindowStrip     = "What you type goes into this run; paste from this Mac does not. Sign-ins stay on the run's disk until it ends. Close this window to hand back; the run keeps going."
	KeptWindowStrip = "What you type goes into this run; paste from this Mac does not. Sign-ins stay on this machine while this machine is kept. Close this window to hand back; the run keeps going."
)

// WindowText is a window's text: its title and the strip under the title bar. A run window has only those.
// The edit window also has CodeFile, the setup-code file's path (not secret), which gives the strip a Show
// code control that reads the code from the file on a click (the code never crosses into Go), and AskStop,
// which its close button calls, off the main thread, instead of closing: it asks the guest to shut down, and
// the window stays up until Close runs. Its buttons call AdminTerminal, TrustCAs, FullDiskAccess and InstallRosetta the same
// way (nil: the click does nothing); CanTrust false disables Trust the list's CAs, which then has nothing to
// trust. Its Type code button needs no callback: it reads the code from CodeFile, as Show code does.
type WindowText struct {
	Title, Strip                                            string
	CodeFile                                                string
	AskStop                                                 func()
	AdminTerminal, TrustCAs, FullDiskAccess, InstallRosetta func()
	CanTrust                                                bool
}

// ErrNotAtMac is what WindowScreen.Ready wraps when the Mac's screen is locked or its console session is not
// this process's. ErrLockUnknown is its answer when it can't tell whether the screen is locked. Any other Ready
// error means the check could not be read; all three refuse a window, and close an open one.
var (
	ErrNotAtMac    = errors.New("macvm: this Mac's screen is locked or its console session is not this user's")
	ErrLockUnknown = errors.New("macvm: whether this Mac's screen is locked can't be told")
)

// errConsoleUnreadable is the console check's answer when the session's on-console key is missing or not a boolean.
var errConsoleUnreadable = errors.New("macvm: the Mac's console session could not be read")

// consoleKey is one boolean key of the Mac's session dictionary as the cgo reader found it. The values are the
// reader's own numbers (wardynScreenConsoleKeys in screen_darwin.m).
type consoleKey int

const (
	keyAbsent    consoleKey = 0
	keyFalse     consoleKey = 1
	keyTrue      consoleKey = 2
	keyMalformed consoleKey = 3 // present but not a boolean; any number the reader does not give reads so too
)

// consoleReading is what the console check read: the session dictionary (Dict false when it could not be had)
// with its on-console and lock keys, and whether the Mac's latest screen notification was a lock.
type consoleReading struct {
	Dict              bool
	OnConsole, Locked consoleKey
	LockNoticed       bool
}

// lockState is what the Mac says of its screen lock. A lock notification, or a lock key that reads true, is locked.
// A session dictionary whose lock key reads false, or has no lock key, is unlocked: macOS adds the key only while
// the screen is locked, so an unlocked Mac usually has none. Anything else (no dictionary, a key that is not a
// boolean) is unknown.
type lockState int

const (
	lockUnknown lockState = iota
	lockUnlocked
	lockLocked
)

func (r consoleReading) lock() lockState {
	switch {
	case r.LockNoticed, r.Dict && r.Locked == keyTrue:
		return lockLocked
	case r.Dict && (r.Locked == keyFalse || r.Locked == keyAbsent):
		return lockUnlocked
	}
	return lockUnknown
}

// consoleCheck is WindowScreen.Ready's answer for r: nil only when the screen is known unlocked and this
// process's session is the Mac's console. A locked screen, or a session not on the console, wraps ErrNotAtMac;
// an unknown lock (no dictionary, or a lock key that is not a boolean) wraps ErrLockUnknown, so it refuses and
// never falls back to the on-console key.
func consoleCheck(r consoleReading) error {
	if r.lock() == lockLocked {
		return fmt.Errorf("the screen is locked: %w", ErrNotAtMac)
	}
	if !r.Dict {
		return fmt.Errorf("no session dictionary: %w", ErrLockUnknown)
	}
	switch r.OnConsole {
	case keyFalse:
		return fmt.Errorf("the session is not on the console: %w", ErrNotAtMac)
	case keyTrue:
	default:
		return errConsoleUnreadable
	}
	switch r.Locked {
	case keyFalse, keyAbsent: // macOS adds the key only while the screen is locked
		return nil
	}
	return fmt.Errorf("the session's CGSSessionScreenIsLocked key is not a boolean: %w", ErrLockUnknown)
}

// consoleKeyOf is the reader's number n as a consoleKey.
func consoleKeyOf(n int) consoleKey {
	if k := consoleKey(n); k >= keyAbsent && k <= keyTrue {
		return k
	}
	return keyMalformed
}

// WindowScreen draws a run's window on the Mac. The darwin cgo build supplies one (nativeWindowScreen); tests
// supply a fake through RunOptions.Window.
type WindowScreen interface {
	// Ready is nil when a window can open now: the screen is known unlocked and this process's session is the
	// Mac's console. It is asked before a window opens and every 2 s while one is up.
	Ready() error
	// Open puts a close-only window on vm (a VZVirtualMachine the caller keeps alive until the window ends).
	Open(vm unsafe.Pointer, text WindowText) (Window, error)
}

// Window is one open run window.
type Window interface {
	// Closed is closed when the person closes the window (or Close has run).
	Closed() <-chan struct{}
	// Locked is closed when the Mac says its screen locked while the window was up; Closed follows. The window
	// then ends as the Mac locking, whatever the session dictionary says.
	Locked() <-chan struct{}
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

// nativeEditScreen is the Mac's edit window, a WindowScreen whose Ready is setup's window check (a window-server
// session, not root) and whose window neither checks the console nor ends on a screen lock: the person started
// edit-base at this Mac, and the window stays until the guest shuts down. Set by the darwin cgo build.
var nativeEditScreen WindowScreen

// watchWindow ends a native window's channels, polling every interval. locked is closed once lockNotices() is no
// longer since, its count when the window began to open: a lock notification came (the C side then tears the
// window down itself). closed is closed once up() is no longer positive: the window's state goes to 0 when the
// person closes it, Close ran or a lock tore it down, and to -1 if it could not be made. up is read before
// lockNotices, and a lock counts its notification before its teardown, so a window a lock closed shows locked
// before closed.
func watchWindow(up func() int, lockNotices func() int64, since int64, interval time.Duration, closed, locked chan<- struct{}) {
	go func() {
		noticed := false
		for {
			isUp := up() > 0
			if !noticed && lockNotices() != since {
				noticed = true
				close(locked)
			}
			if !isUp {
				close(closed)
				return
			}
			time.Sleep(interval)
		}
	}()
}
