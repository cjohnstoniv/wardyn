// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package foldersync

import (
	"os"
	"path"

	"golang.org/x/sys/unix"
)

// exchangeFiles swaps the entries a and b of one directory in one step (renameatx_np with
// RENAME_SWAP): neither name is ever free and neither file is unlinked. It fails, swapping nothing,
// where the filesystem does not offer it. Replaced by tests.
var exchangeFiles = func(root *os.Root, a, b string) error {
	d, err := openFileIn(root, path.Dir(a), os.O_RDONLY)
	if err != nil {
		return err
	}
	defer d.Close()
	return unix.RenameatxNp(int(d.Fd()), path.Base(a), int(d.Fd()), path.Base(b), unix.RENAME_SWAP)
}
