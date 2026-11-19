// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"os"
	"path/filepath"
	"strings"
)

// OpenFromTop opens the directory at the absolute path real one directory at a time from the top of
// its volume (OpenDir), following no link at any component. The handle is the directory real names
// with no link in it when it is opened: a folder on the way renamed or replaced by a link after real
// was checked refuses the open, and is never opened through. Opening a directory needs read access
// to it, so a folder on the way this user can pass through but not read refuses the open too. The
// caller closes the handle.
func OpenFromTop(real string) (*os.Root, error) {
	top := filepath.VolumeName(real) + string(filepath.Separator)
	rel, err := filepath.Rel(top, real)
	if err != nil {
		return nil, err
	}
	cur, err := os.OpenRoot(top)
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(filepath.ToSlash(rel), "/") {
		next, err := OpenDir(cur, name)
		_ = cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}
