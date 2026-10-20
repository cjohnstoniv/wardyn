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
