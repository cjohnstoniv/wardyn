// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package macvm

/*
#cgo LDFLAGS: -framework AppKit -framework CoreGraphics
#include <pthread.h>
#include <stdlib.h>

int wardynScreenSession(void);
void wardynScreenRun(void);
void wardynScreenShow(void *vm, const char *title, const char *steps, const char *codeFile);
void wardynScreenShowWindow(void *vm, const char *title, const char *steps, const char *codeFile, int askStop, int canTrust);
void wardynScreenSetLive(const char *text);
int wardynScreenConsoleKeys(int *onConsole, int *locked);
long wardynScreenLockNotices(void);
int wardynScreenLockNoticed(void);
int wardynScreenUp(void);
void wardynScreenClose(void);
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

func init() {
	serveScreens = serveScreensDarwin
	nativeWindowScreen = macWindowScreen{}
	nativeEditScreen = macEditScreen{}
}

var (
	// screensServed: ServeScreens runs on the main thread, which is free for AppKit.
	screensServed atomic.Bool
	// screenStart is closed by the first window: the main thread then runs AppKit.
	screenStart     = make(chan struct{})
	screenStartOnce sync.Once
)

// errNoScreen is Screen's answer where nothing serves the main thread (not the helper's main).
var errNoScreen = errors.New("macvm: no window can open: this process's main thread is not serving AppKit")

// screenWait bounds how long showScreen waits for the main thread to put the window up.
const screenWait = 10 * time.Second

func serveScreensDarwin(work func() int) int {
	if C.pthread_main_np() == 0 {
		return work()
	}
	screensServed.Store(true)
	code := make(chan int, 1)
	go func() { code <- work() }()
	select {
	case c := <-code:
		return c
	case <-screenStart:
	}
	go func() { os.Exit(<-code) }()
	C.wardynScreenRun() // AppKit's run loop; it does not return
	return <-code
}

// screenReady is nil when showScreen can open a window: the main thread serves AppKit, the process is not
// root, and it has a window-server session (none with nobody at the desktop; runnerd's launchctl check refuses SSH).
func screenReady() error {
	if !screensServed.Load() {
		return errNoScreen
	}
	if geteuid() == 0 {
		return fmt.Errorf("prepare runs as root: %w", ErrNoDesktopSession)
	}
	if C.wardynScreenSession() == 0 {
		return fmt.Errorf("this process has no window-server session: %w", ErrNoDesktopSession)
	}
	return nil
}

// showScreen opens the window on vm (a VZVirtualMachine the caller keeps alive until unlock), with
// text, and returns its close. text.CodeFile is only a path: the window reads the code from
// it on a click, and the code never crosses this call. It refuses before AppKit starts when screenReady does. The
// window is made on the main thread once AppKit runs there, and showScreen returns once it is up.
func showScreen(vm unsafe.Pointer, text ScreenText, unlock func()) (func(), error) {
	if err := screenReady(); err != nil {
		unlock()
		return nil, err
	}
	var title, steps, codeFile *C.char // NULL: setup by hand's text; no Show code control
	if text.Title != "" {
		title = C.CString(text.Title)
		defer C.free(unsafe.Pointer(title))
	}
	if text.Steps != "" {
		steps = C.CString(text.Steps)
		defer C.free(unsafe.Pointer(steps))
	}
	if text.CodeFile != "" {
		codeFile = C.CString(text.CodeFile)
		defer C.free(unsafe.Pointer(codeFile))
	}
	C.wardynScreenShow(vm, title, steps, codeFile)
	unlock() // the window's block holds vm now
	screenStartOnce.Do(func() { close(screenStart) })
	var once sync.Once
	closeWindow := func() { once.Do(func() { C.wardynScreenClose() }) }
	// ponytail: polled, since the main queue answers once; a Go callback from C if more ever needs it.
	for deadline := time.Now().Add(screenWait); C.wardynScreenUp() == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			closeWindow()
			return nil, fmt.Errorf("macvm: the guest's window did not open within %v", screenWait)
		}
	}
	if C.wardynScreenUp() < 0 {
		closeWindow()
		return nil, errors.New("macvm: the guest's window could not be made")
	}
	return closeWindow, nil
}

// macWindowScreen is the Mac's WindowScreen: a close-only window on the helper's main thread.
type macWindowScreen struct{}

// Ready is nil when the helper can draw a window and its session is the Mac's console, known unlocked. The cgo
// reader gives the session dictionary's keys and the latest lock notification; consoleCheck decides (screen.go).
func (macWindowScreen) Ready() error {
	if !screensServed.Load() {
		return errNoScreen
	}
	if geteuid() == 0 {
		return fmt.Errorf("the helper runs as root: %w", ErrNotAtMac)
	}
	r := consoleReading{LockNoticed: C.wardynScreenLockNoticed() != 0}
	var onConsole, locked C.int
	if C.wardynScreenConsoleKeys(&onConsole, &locked) != 0 {
		r.Dict, r.OnConsole, r.Locked = true, consoleKeyOf(int(onConsole)), consoleKeyOf(int(locked))
	}
	return consoleCheck(r)
}

// Open puts the run window up on vm and returns once it is. The window ends when the person closes it or Close
// runs; neither touches the VM.
func (macWindowScreen) Open(vm unsafe.Pointer, text WindowText) (Window, error) {
	// since is read first and the console check runs again after it: a lock that lands after the check moves the
	// count past since and ends the window at once, and one before it is refused by the check itself. The caller's
	// own Ready ran before since was read, so it alone cannot cover that gap. The check after the window is up covers a
	// lock before the observer existed (the first window): the observer installs after screenStart closes, and a
	// notification is not replayed.
	since := int64(C.wardynScreenLockNotices())
	if err := screenReady(); err != nil {
		return nil, err
	}
	if err := (macWindowScreen{}).Ready(); err != nil {
		return nil, err
	}
	if C.wardynScreenUp() != 0 {
		return nil, errors.New("macvm: a window is already open in this process")
	}
	title, steps := C.CString(text.Title), C.CString(text.Strip)
	defer C.free(unsafe.Pointer(title))
	defer C.free(unsafe.Pointer(steps))
	C.wardynScreenShowWindow(vm, title, steps, nil, 0, 0)
	screenStartOnce.Do(func() { close(screenStart) })
	w := &macWindow{closed: make(chan struct{}), locked: make(chan struct{})}
	for deadline := time.Now().Add(screenWait); C.wardynScreenUp() == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			w.Close()
			return nil, fmt.Errorf("macvm: the run's window did not open within %v", screenWait)
		}
	}
	if C.wardynScreenUp() < 0 {
		w.Close()
		return nil, errors.New("macvm: the run's window could not be made")
	}
	if err := (macWindowScreen{}).Ready(); err != nil {
		w.Close()
		return nil, err
	}
	// ponytail: polled, as showScreen's is; the close-only delegate and the lock observer set the state to 0 on
	// the main queue, and the lock observer counts its notification first.
	watchWindow(func() int { return int(C.wardynScreenUp()) }, func() int64 { return int64(C.wardynScreenLockNotices()) },
		since, 50*time.Millisecond, w.closed, w.locked)
	return w, nil
}

type macWindow struct {
	closed, locked chan struct{}
	once           sync.Once
	text           *WindowText // the edit window's callbacks (windowText), nil for a run window
}

func (w *macWindow) Closed() <-chan struct{} { return w.closed }
func (w *macWindow) Locked() <-chan struct{} { return w.locked }

// Close detaches the view and closes the window, and returns once AppKit has (or after screenWait), so the
// next window finds the slot empty.
func (w *macWindow) Close() {
	w.once.Do(func() {
		if w.text != nil {
			windowText.CompareAndSwap(w.text, nil)
		}
		C.wardynScreenClose()
		for deadline := time.Now().Add(screenWait); C.wardynScreenUp() != 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// windowText is the open edit window's text, whose callbacks its controls call (wardynWindowAction).
var windowText atomic.Pointer[WindowText]

// The numbers the edit window's controls pass to wardynWindowAction (WARDYN_WINDOW_* in screen_darwin.m): its
// close button, then the strip's buttons. Type code has none: it never leaves the window.
const (
	windowActionAskStop = iota
	windowActionAdminTerminal
	windowActionTrustCAs
	windowActionFullDiskAccess
	windowActionInstallRosetta
)

// wardynWindowAction runs the callback of the open edit window's control n. The window calls it off the
// main thread, so a callback may wait on the guest.
//
//export wardynWindowAction
func wardynWindowAction(n C.int) {
	t := windowText.Load()
	if t == nil {
		return
	}
	f := map[int]func(){
		windowActionAskStop:        t.AskStop,
		windowActionAdminTerminal:  t.AdminTerminal,
		windowActionTrustCAs:       t.TrustCAs,
		windowActionFullDiskAccess: t.FullDiskAccess,
		windowActionInstallRosetta: t.InstallRosetta,
	}[int(n)]
	if f != nil {
		f()
	}
}

// ShowLive sets the edit window's live line (the guest's read and what the last button did); a run window,
// or one already closed, has none.
func (w *macWindow) ShowLive(text string) {
	if w.text == nil || windowText.Load() != w.text {
		return
	}
	cs := C.CString(text)
	defer C.free(unsafe.Pointer(cs))
	C.wardynScreenSetLive(cs)
}

// macEditScreen is the edit window: the run window's, with the edit's strip and Show code control and a close
// button that calls text.AskStop. It checks only what setup's window checks (screenReady): the person started
// edit-base at this Mac, so neither the console check nor a screen lock ends it, and it stays up until the
// guest has shut down and Close runs.
type macEditScreen struct{}

func (macEditScreen) Ready() error { return screenReady() }

func (macEditScreen) Open(vm unsafe.Pointer, text WindowText) (Window, error) {
	if err := screenReady(); err != nil {
		return nil, err
	}
	if C.wardynScreenUp() != 0 {
		return nil, errors.New("macvm: a window is already open in this process")
	}
	title, steps := C.CString(text.Title), C.CString(text.Strip)
	defer C.free(unsafe.Pointer(title))
	defer C.free(unsafe.Pointer(steps))
	var codeFile *C.char // NULL: no Show code control
	if text.CodeFile != "" {
		codeFile = C.CString(text.CodeFile)
		defer C.free(unsafe.Pointer(codeFile))
	}
	w := &macWindow{closed: make(chan struct{}), locked: make(chan struct{}), text: &text}
	windowText.Store(w.text)
	canTrust := C.int(0)
	if text.CanTrust {
		canTrust = 1
	}
	C.wardynScreenShowWindow(vm, title, steps, codeFile, 1, canTrust)
	screenStartOnce.Do(func() { close(screenStart) })
	for deadline := time.Now().Add(screenWait); C.wardynScreenUp() == 0; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			w.Close()
			return nil, fmt.Errorf("macvm: the edit window did not open within %v", screenWait)
		}
	}
	if C.wardynScreenUp() < 0 {
		w.Close()
		return nil, errors.New("macvm: the edit window could not be made")
	}
	// No lock count: a lock leaves the edit window up, so Locked never closes.
	watchWindow(func() int { return int(C.wardynScreenUp()) }, func() int64 { return 0 }, 0, 50*time.Millisecond, w.closed, w.locked)
	return w, nil
}
