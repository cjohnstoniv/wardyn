// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package foldersync

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"

	"golang.org/x/sys/windows"
)

// replaceFile first asks Windows to replace to and write the move through to the disk, since a
// directory cannot be synced there; failing that it renames through root.
func replaceFile(root *os.Root, from, to string) error {
	if src, dst, err := nativePaths(root, from, to); err == nil {
		if windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH) == nil {
			return nil
		}
	}
	return root.Rename(from, to)
}

// linkNew fails with fs.ErrExist when to exists. from stays open, so it is linked, not moved.
func linkNew(root *os.Root, from, to string) error {
	if src, dst, err := nativePaths(root, from, to); err == nil {
		err := windows.CreateHardLink(dst, src, 0)
		if err == nil {
			return nil
		}
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return fmt.Errorf("link %s: %w", to, fs.ErrExist)
		}
	}
	return root.Link(from, to)
}

// nativePaths names from and to (in one directory of root) by that directory's resolved path, so
// no link swapped in above it is followed.
func nativePaths(root *os.Root, from, to string) (src, dst *uint16, err error) {
	if path.Dir(from) != path.Dir(to) {
		return nil, nil, errors.New("not in one directory")
	}
	d, err := root.Open(path.Dir(from))
	if err != nil {
		return nil, nil, err
	}
	defer d.Close()
	buf := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(d.Fd()), &buf[0], uint32(len(buf)), 0)
	if err != nil {
		return nil, nil, err
	}
	if int(n) >= len(buf) {
		return nil, nil, errors.New("directory path too long")
	}
	dir := windows.UTF16ToString(buf[:n])
	if src, err = windows.UTF16PtrFromString(dir + `\` + path.Base(from)); err != nil {
		return nil, nil, err
	}
	dst, err = windows.UTF16PtrFromString(dir + `\` + path.Base(to))
	return src, dst, err
}

// Windows has no directory sync through os.File; the write-through move stands for it, and its
// absence never fails an install.
func syncDirIn(*os.Root, string) error { return nil }

func syncDirPath(string) error { return nil }
