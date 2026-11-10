// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import (
	"errors"
	"os"
	"syscall"
)

// existingSyncFlag opens a file only to read and sync it.
const existingSyncFlag = os.O_RDONLY

func replaceFile(root *os.Root, from, to string) error { return root.Rename(from, to) }

// placeNew links to at from; it fails with fs.ErrExist when to exists.
func placeNew(root *os.Root, from, to string) error { return root.Link(from, to) }

func syncDirIn(root *os.Root, note *dirNote, dir string) error {
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return dirSynced(note, dir, fsyncDir(d))
}

func syncDirPath(note *dirNote, dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return dirSynced(note, dir, fsyncDir(d))
}

// dirSynced is a directory sync's error for the caller: one the filesystem does not offer is noted
// and is nil, and every other error stands.
func dirSynced(note *dirNote, dir string, err error) error {
	if dirSyncUnsupported(err) {
		note.unsupported(dir, err)
		return nil
	}
	return err
}

// dirSyncUnsupported is a directory sync the filesystem does not offer (EINVAL, ENOTSUP or
// EOPNOTSUPP, as from some network and FUSE filesystems). Any other error, EIO and ENOSPC among
// them, is a failed sync.
func dirSyncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}
