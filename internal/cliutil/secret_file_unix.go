// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package cliutil

import (
	"os"
	"syscall"
)

// fileOwnerUID is the file's owning uid, or -1 when the platform does not say.
func fileOwnerUID(fi os.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}
