// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package admission

import "golang.org/x/sys/unix"

// renameNoReplace renames from in fromfd to to in tofd, in one step, and
// fails with EEXIST if to is there.
func renameNoReplace(fromfd int, from string, tofd int, to string) error {
	return unix.RenameatxNp(fromfd, from, tofd, to, unix.RENAME_EXCL)
}

// renameSwap exchanges a and b in dirfd, in one step.
func renameSwap(dirfd int, a, b string) error {
	return unix.RenameatxNp(dirfd, a, dirfd, b, unix.RENAME_SWAP)
}
