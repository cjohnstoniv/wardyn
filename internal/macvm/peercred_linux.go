// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the uid of the process at c's other end (SO_PEERCRED; Linux runs the fake engine).
func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid int
	var serr error
	if err := raw.Control(func(fd uintptr) {
		var cred *unix.Ucred
		if cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); serr == nil {
			uid = int(cred.Uid)
		}
	}); err != nil {
		return 0, err
	}
	return uid, serr
}
