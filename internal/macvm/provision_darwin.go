// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package macvm

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation -framework Virtualization
#include <stdlib.h>

char *wardynProvisionMissing(void);
void *wardynProvisionStart(const char *disk, const char *aux, const void *hw, int hwLen, const void *mid, int midLen,
                           unsigned cpus, unsigned long long mem, int fd, const char *user, const char *pass, char **errOut);
char *wardynProvisionWait(void *h);
void wardynProvisionRequestStop(void *h);
void wardynProvisionForceStop(void *h);
void wardynProvisionFree(void *h);
void *wardynProvisionMachine(void *h);
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"
)

func init() {
	provisionPlatform = func() Provisioner { return provisionDarwin{} }
}

// provisionDarwin's screen is its boots' window text; the zero value is setup by hand's.
type provisionDarwin struct{ screen ScreenText }

var _ screenTexter = provisionDarwin{}

// WithScreen is a provisioner whose boots' window shows text.
func (p provisionDarwin) WithScreen(text ScreenText) FirstBooter {
	p.screen = text
	return p
}

func (provisionDarwin) Supported() error {
	if m := C.wardynProvisionMissing(); m != nil {
		defer C.free(unsafe.Pointer(m))
		missing := C.GoString(m)
		if missing == ErrNoVirtualization.Error() {
			return fmt.Errorf("macvm provision: this Mac's Virtualization framework has no %w", ErrNoVirtualization)
		}
		return fmt.Errorf("macvm provision: this Mac's Virtualization framework has no %s; %w needs macOS %d or later", missing, ErrNoProvisioning, ProvisionHostMajor)
	}
	return nil
}

func (provisionDarwin) ScreenReady() error { return screenReady() }

// Boot with username "" is the plain boot of a host without guest provisioning; the C side then
// starts the VM with no provisioning options.
func (p provisionDarwin) Boot(disk, aux string, hardwareModel, machineID []byte, cpus uint, memoryBytes uint64, nic *os.File, username, password string) (FirstBoot, error) {
	if err := p.Supported(); err != nil && (username != "" || !errors.Is(err, ErrNoProvisioning)) {
		return FirstBoot{}, err
	}
	if len(hardwareModel) == 0 || len(machineID) == 0 {
		return FirstBoot{}, errors.New("macvm provision: the base has no hardware model or machine identifier")
	}
	raw, err := nic.SyscallConn()
	if err != nil {
		return FirstBoot{}, fmt.Errorf("macvm provision: network socket: %w", err)
	}
	cs := []*C.char{C.CString(disk), C.CString(aux), C.CString(username), C.CString(password)}
	defer func() {
		for _, c := range cs {
			C.free(unsafe.Pointer(c))
		}
	}()

	var h unsafe.Pointer
	var cErr *C.char
	if err := raw.Control(func(fd uintptr) {
		h = C.wardynProvisionStart(cs[0], cs[1],
			unsafe.Pointer(&hardwareModel[0]), C.int(len(hardwareModel)),
			unsafe.Pointer(&machineID[0]), C.int(len(machineID)),
			C.uint(cpus), C.ulonglong(memoryBytes), C.int(fd), cs[2], cs[3], &cErr)
	}); err != nil {
		return FirstBoot{}, fmt.Errorf("macvm provision: network socket: %w", err)
	}
	if h == nil {
		return FirstBoot{}, classifyProvisionStart(provisionCError("start", cErr))
	}

	vm := &provisionVM{h: h, text: p.screen, done: make(chan error, 1), exited: make(chan struct{})}
	go vm.wait()
	return FirstBoot{Done: vm.done, Stop: vm.stop, Screen: vm.screen}, nil
}

// provisionVM owns one started VM. h is freed, under mu, once the VM stops.
type provisionVM struct {
	mu     sync.Mutex
	h      unsafe.Pointer
	text   ScreenText
	result error
	done   chan error
	exited chan struct{}
}

func (v *provisionVM) wait() {
	err := provisionCError("VM stopped", C.wardynProvisionWait(v.h))
	v.mu.Lock()
	C.wardynProvisionFree(v.h)
	v.h = nil
	v.result = err
	v.mu.Unlock()
	close(v.exited)
	v.done <- err
}

func (v *provisionVM) stop(ctx context.Context) error {
	v.call(func(h unsafe.Pointer) { C.wardynProvisionRequestStop(h) })
	select {
	case <-v.exited:
	case <-ctx.Done():
		v.call(func(h unsafe.Pointer) { C.wardynProvisionForceStop(h) })
		<-v.exited
	}
	return v.result
}

// screen opens this VM's window (showScreen), with its text. The lock keeps the VM and its handle from
// being freed until the window's block holds them.
func (v *provisionVM) screen() (func(), error) {
	v.mu.Lock()
	if v.h == nil {
		v.mu.Unlock()
		return nil, ErrGuestStopped
	}
	return showScreen(C.wardynProvisionMachine(v.h), v.h, v.text, v.mu.Unlock)
}

func (v *provisionVM) call(f func(unsafe.Pointer)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.h != nil {
		f(v.h)
	}
}

// provisionCError turns a C error string into an error and frees it; NULL is nil.
func provisionCError(what string, msg *C.char) error {
	if msg == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(msg))
	return fmt.Errorf("macvm provision: %s: %s", what, C.GoString(msg))
}
