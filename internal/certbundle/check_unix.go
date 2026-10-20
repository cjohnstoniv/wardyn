// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package certbundle

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// CheckFile refuses an anchor file that has any write bit, is owned by an
// account other than the caller's or root, or sits in a directory that group
// or others can write. It stops other accounts and careless tools, not the
// same account. The refusal names the file and the mode.
func CheckFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if m := fi.Mode().Perm(); m&0o222 != 0 {
		return fmt.Errorf("refusing the CA file %s: mode %04o has a write bit (want 0400)", path, m)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if uid := int(st.Uid); uid != os.Getuid() && uid != 0 {
			return fmt.Errorf("refusing the CA file %s: mode %04o, owned by uid %d, not this account (uid %d) or root",
				path, fi.Mode().Perm(), uid, os.Getuid())
		}
	}
	dir := filepath.Dir(path)
	di, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if m := di.Mode().Perm(); m&0o022 != 0 {
		return fmt.Errorf("refusing the CA file %s: its directory %s has mode %04o, writable by group or others (want 0700)",
			path, dir, m)
	}
	return nil
}
