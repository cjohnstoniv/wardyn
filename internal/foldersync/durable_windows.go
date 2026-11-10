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

// existingSyncFlag opens a file only to read and sync it: FlushFileBuffers needs write access. It
// neither creates nor truncates.
const existingSyncFlag = os.O_RDWR

// replaceFile moves from over to and has Windows write the move through to the disk, since a
// directory cannot be synced there. Only when the directory's path cannot be resolved does it fall
// back to a rename through root.
func replaceFile(root *os.Root, from, to string) error {
	src, dst, err := nativePaths(root, from, to)
	if err != nil {
		return root.Rename(from, to)
	}
	return windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// placeNew moves from to to, written through, never replacing to: it fails with fs.ErrExist when
// to exists. A hard link is not used, as it has no write-through flag.
func placeNew(root *os.Root, from, to string) error {
	src, dst, err := nativePaths(root, from, to)
	if err != nil {
		return err
	}
	err = windows.MoveFileEx(src, dst, windows.MOVEFILE_WRITE_THROUGH)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return fmt.Errorf("move to %s: %w", to, fs.ErrExist)
	}
	return err
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
func syncDirIn(*os.Root, *dirNote, string) error { return nil }

func syncDirPath(*dirNote, string) error { return nil }
