// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package guestagent

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// cloexecSocket makes a socket that no exec inherits. Darwin has no SOCK_CLOEXEC and Go's darwin
// fork does not sweep descriptors, so without this the agent's untrusted children would hold the
// control and desktop connections to the host. ForkLock keeps a concurrent fork from running between
// the socket and the flag.
func cloexecSocket(domain, typ, proto int) (int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fd, err := unix.Socket(domain, typ, proto)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	return fd, err
}
