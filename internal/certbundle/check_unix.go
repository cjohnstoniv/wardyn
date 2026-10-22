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

// anchorOwner is fileOwner, a var so a test can stand in root or another account.
var anchorOwner = fileOwner

// fileOwner is the uid that owns fi.
func fileOwner(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// openChecked opens an anchor file once and checks it on that descriptor:
//   - the directory holding path as given, and the one holding its resolved
//     target, are not group- or other-writable unless sticky and root-owned;
//   - the target, opened without following a final symlink, is a regular file
//     owned by this account (euid) or root, with no group or other write bit.
//
// It stops other accounts and careless tools, not the same account. A refusal
// names the file and the mode.
func openChecked(path string) (*os.File, error) {
	if err := checkDir(filepath.Dir(path), path); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if err := checkDir(filepath.Dir(resolved), path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("refusing the CA file %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err == nil {
		err = checkFileInfo(path, fi)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func checkFileInfo(path string, fi os.FileInfo) error {
	m := fi.Mode()
	if !m.IsRegular() {
		return fmt.Errorf("refusing the CA file %s: not a regular file (mode %v)", path, m)
	}
	if m.Perm()&0o022 != 0 {
		return fmt.Errorf("refusing the CA file %s: mode %04o is writable by group or others", path, m.Perm())
	}
	if uid, ok := anchorOwner(fi); ok && uid != os.Geteuid() && uid != 0 {
		return fmt.Errorf("refusing the CA file %s: mode %04o, owned by uid %d, not this account (uid %d) or root",
			path, m.Perm(), uid, os.Geteuid())
	}
	return nil
}

func checkDir(dir, path string) error {
	di, err := os.Stat(dir)
	if err != nil {
		return err
	}
	m := di.Mode()
	if m.Perm()&0o022 == 0 {
		return nil
	}
	if uid, ok := anchorOwner(di); ok && uid == 0 && m&os.ModeSticky != 0 {
		return nil
	}
	return fmt.Errorf("refusing the CA file %s: its directory %s has mode %04o, writable by group or others and not a sticky root-owned directory",
		path, dir, m.Perm())
}
