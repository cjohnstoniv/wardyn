// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package macvm

import "golang.org/x/sys/unix"

// cloneFile is clonefile(2): an APFS copy-on-write clone, so later writes to
// either file are private to it. It does not follow a symlink at src.
func cloneFile(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
