// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import "syscall"

// markTokenFD marks an inherited /dev/fd/N token descriptor close-on-exec, so even a child started
// before it is read and closed cannot inherit it.
func markTokenFD(path string) {
	if n, ok := tokenFD(path); ok {
		syscall.CloseOnExec(n)
	}
}

func closeFD(n int) { _ = syscall.Close(n) }
