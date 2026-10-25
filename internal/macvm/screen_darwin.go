// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package macvm

/*
#cgo LDFLAGS: -framework AppKit -framework CoreGraphics
#include <pthread.h>

int wardynScreenSession(void);
void wardynScreenRun(void);
void wardynScreenShow(void *vm);
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

func init() { serveScreens = serveScreensDarwin }

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

// screenReady is nil when showScreen can open a window: the main thread serves AppKit, and the process
// is in a window-server session (CoreGraphics' CGSessionCopyCurrentDictionary; none over SSH).
func screenReady() error {
	if !screensServed.Load() {
		return errNoScreen
	}
	if C.wardynScreenSession() == 0 {
		return fmt.Errorf("macvm: this process has no window-server session: %w", ErrNoDesktopSession)
	}
	return nil
}

// showScreen opens the window on vm (a VZVirtualMachine the caller keeps alive for this call) and
// returns its close. It refuses before AppKit starts when screenReady does. The window is made on the
// main thread once AppKit runs there, and showScreen returns once it is up.
func showScreen(vm unsafe.Pointer) (func(), error) {
	if err := screenReady(); err != nil {
		return nil, err
	}
	C.wardynScreenShow(vm)
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
