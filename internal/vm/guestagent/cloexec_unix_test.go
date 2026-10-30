// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package guestagent

import (
	"fmt"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCloexecSocketIsCloseOnExec(t *testing.T) {
	fd, err := cloexecSocket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Error("the socket would be inherited by an exec")
	}
}

// A child must not see the descriptor at all (on macOS this is the check that matters).
func TestCloexecSocketIsNotInheritedByAChild(t *testing.T) {
	fd, err := cloexecSocket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := exec.Command("/bin/sh", "-c", fmt.Sprintf("test -e /dev/fd/%d", fd)).Run(); err == nil {
		t.Errorf("the child holds descriptor %d", fd)
	}
}
