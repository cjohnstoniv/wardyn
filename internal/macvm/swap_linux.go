// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package macvm

import "golang.org/x/sys/unix"

// swapDirs atomically exchanges two directories: at no instant is either
// path missing. Only the fake engine's tests reach it; the helper runs on macOS.
func swapDirs(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}
