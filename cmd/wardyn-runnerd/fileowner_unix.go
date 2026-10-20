// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"os"
	"syscall"
)

// fileOwner is the uid that owns the file, when the platform says.
func fileOwner(st os.FileInfo) (int, bool) {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(s.Uid), true
	}
	return 0, false
}

// openOrgCAFile opens the resolved path without following a final link and without blocking: a FIFO there
// would otherwise hold the open forever, and the type check on the descriptor refuses it.
func openOrgCAFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
