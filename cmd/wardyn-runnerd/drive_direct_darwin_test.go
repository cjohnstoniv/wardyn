// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// driveDirectLockedBuffer is a Buffer the command writes while the test polls it.
type driveDirectLockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *driveDirectLockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *driveDirectLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A real image through the real hdiutil, formatted the way the guest agent formats a drive: the flags show in
// mount, a read and a write work, a binary on the volume won't execute, and a real detach clears the marker.
// An attach the machine refuses skips: the test then says NOT RUN, and the case is on the Mac run sheet.
func TestDriveDirectRealImage(t *testing.T) {
	root := filepath.Join("/private/tmp", "wardyn-drive-direct-"+time.Now().UTC().Format("150405.000000"))
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	driveDirectSettingSaver(state)(true)
	m := &macOSVMs{layout: macvm.Layout{Root: root}, limitsFile: macOSVMLimitsFile(state)}
	image, err := m.layout.DriveImage(driveDirectTestObject)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(image), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(image, 64<<20); err != nil {
		t.Fatal(err)
	}
	if err := driveDirectTestNameFile(m); err != nil {
		t.Fatal(err)
	}
	blank, err := exec.Command("hdiutil", "attach", "-nomount", "-imagekey", "diskimage-class=CRawDiskImage", image).CombinedOutput()
	if err != nil {
		t.Skipf("NOT RUN: this machine refuses hdiutil attach: %v: %s", err, blank)
	}
	dev := regexp.MustCompile(`/dev/disk[0-9]+`).FindString(string(blank))
	if dev == "" {
		t.Fatalf("attach said %s", blank)
	}
	const label = "WardynDirectTest"
	erased, eraseErr := exec.Command("diskutil", "eraseDisk", "JHFS+", label, "GPT", dev).CombinedOutput()
	if out, err := exec.Command("hdiutil", "detach", dev).CombinedOutput(); err != nil {
		t.Fatalf("detach: %v: %s", err, out)
	}
	if eraseErr != nil {
		t.Skipf("NOT RUN: this machine refuses diskutil eraseDisk: %v: %s", eraseErr, erased)
	}
	// Nothing attached: the real hdiutil's empty listing must parse, or no recovery could ever clear.
	if devs, err := driveDirectAttached(image); err != nil || len(devs) != 0 {
		t.Fatalf("driveDirectAttached with nothing attached = %v, %v", devs, err)
	}

	in, press := io.Pipe()
	defer press.Close()
	out := &driveDirectLockedBuffer{}
	done := make(chan error, 1)
	a := driveDirectArgs{name: "scratch", in: in, out: out}
	go func() { done <- m.driveDirect(context.Background(), &driveDirectFlags{direct: true}, a) }()
	deadline := time.Now().Add(2 * time.Minute)
	for !strings.Contains(out.String(), "Press Return") {
		select {
		case err := <-done:
			t.Fatalf("command ended early: %v\n%s", err, out.String())
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("no mount in time:\n%s", out.String())
		}
	}
	point := "/Volumes/" + label
	table, err := exec.Command("mount").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("mount says: %s", table)
	if !driveDirectFlagsHeld(string(table), point) {
		t.Errorf("mount lacks the flags on %s:\n%s", point, table)
	}
	if _, err := macvm.LockDisk(image); !errors.Is(err, macvm.ErrDiskInUse) {
		t.Errorf("LockDisk while mounted = %v", err)
	}
	file := filepath.Join(point, "probe.sh")
	if err := os.WriteFile(file, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Errorf("write on the volume: %v", err)
	}
	if data, err := os.ReadFile(file); err != nil || !strings.Contains(string(data), "exit 0") {
		t.Errorf("read on the volume: %q, %v", data, err)
	}
	if err := exec.Command(file).Run(); err == nil {
		t.Error("a program on the volume ran: noexec did not hold")
	}
	if _, err := press.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("close: %v\n%s", err, out.String())
	}
	if devs, err := driveDirectAttached(image); err != nil || len(devs) != 0 {
		t.Fatalf("after the close = %v, %v", devs, err)
	}
	unlock, err := macvm.LockDisk(image)
	if err != nil {
		t.Fatalf("LockDisk after the close = %v", err)
	}
	unlock()

	// A killed command's state: the marker there and the image attached. Recovery detaches it and clears.
	if err := macvm.WriteDiskMount(image, macvm.DiskMount{Phase: macvm.DiskMountAttached, Name: "scratch", Image: image}); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("hdiutil", "attach", "-nobrowse", "-noautoopen", "-imagekey", "diskimage-class=CRawDiskImage", image).CombinedOutput(); err != nil {
		t.Fatalf("attach: %v: %s", err, b)
	}
	var mounted *macvm.DiskMountedError
	if _, err := macvm.LockDisk(image); !errors.As(err, &mounted) {
		t.Fatalf("LockDisk with a marker = %v", err)
	}
	a.in, a.out = strings.NewReader(""), io.Discard
	if err := m.driveDirect(context.Background(), &driveDirectFlags{direct: true, recover: true}, a); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if devs, err := driveDirectAttached(image); err != nil || len(devs) != 0 {
		t.Fatalf("after recover = %v, %v", devs, err)
	}
}
