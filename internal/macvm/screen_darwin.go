// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package macvm

/*
#cgo LDFLAGS: -framework AppKit
#include <pthread.h>

void wardynScreenRun(void);
void wardynScreenShow(void *vm);
void wardynScreenClose(void);
*/
import "C"

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
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

// showScreen opens the window on vm (a VZVirtualMachine the caller keeps alive for this call) and
// returns its close. The window is made on the main thread once AppKit runs there.
func showScreen(vm unsafe.Pointer) (func(), error) {
	if !screensServed.Load() {
		return nil, errNoScreen
	}
	C.wardynScreenShow(vm)
	screenStartOnce.Do(func() { close(screenStart) })
	var once sync.Once
	return func() { once.Do(func() { C.wardynScreenClose() }) }, nil
}
