// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import "golang.org/x/sys/unix"

func fdPeerUID(fd int) (uint32, error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
