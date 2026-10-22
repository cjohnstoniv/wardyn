// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY returns a pty's slave side as an *os.File that term.IsTerminal
// accepts, and drains the master into the returned buffer.
func openPTY(t *testing.T) (slave *os.File, read func() string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	var unlock, n int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Skipf("unlockpt: %v", e)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Skipf("ptsname: %v", e)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	// Raw mode so the line discipline does not rewrite the bytes.
	var tio syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&tio))); e == 0 {
		tio.Oflag &^= syscall.OPOST
		syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&tio)))
	}
	return slave, func() string {
		buf := make([]byte, 4096)
		k, _ := master.Read(buf)
		return string(buf[:k])
	}
}

func TestRunOutput_TerminalEscapesUnlessRaw(t *testing.T) {
	const payload = "x\x1b]52;c;QUJD\x07y\x1b[31mred\x1b[0m\n"
	body := map[string]any{"output": payload, "complete": true, "source": "stdout"}
	srv := outputServer(t, 200, body)

	slave, read := openPTY(t)
	if _, err := runOutputCLI(t, srv, slave); err != nil {
		t.Fatal(err)
	}
	got := read()
	if strings.ContainsRune(got, 0x1b) || !strings.Contains(got, `\x1b]52;c;QUJD\x07y\x1b[31mred\x1b[0m`) {
		t.Errorf("pty output = %q, want escaped", got)
	}

	slave2, read2 := openPTY(t)
	if _, err := runOutputCLI(t, srv, slave2, "--raw"); err != nil {
		t.Fatal(err)
	}
	if got := read2(); got != payload {
		t.Errorf("--raw pty output = %q, want verbatim", got)
	}

	// A pane snapshot is filtered even with --raw.
	snap := outputServer(t, 200, map[string]any{"output": payload, "complete": true, "source": "pane_snapshot"})
	slave3, read3 := openPTY(t)
	if _, err := runOutputCLI(t, snap, slave3, "--raw"); err != nil {
		t.Fatal(err)
	}
	if got := read3(); strings.ContainsRune(got, 0x1b) {
		t.Errorf("pane snapshot with --raw = %q, want escaped", got)
	}
}
