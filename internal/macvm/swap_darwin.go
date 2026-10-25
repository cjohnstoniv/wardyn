// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package macvm

import "golang.org/x/sys/unix"

// swapDirs atomically exchanges two directories (renamex_np with RENAME_SWAP):
// at no instant is either path missing.
func swapDirs(a, b string) error { return unix.RenamexNp(a, b, unix.RENAME_SWAP) }
