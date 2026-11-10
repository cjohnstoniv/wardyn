// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// existingSyncFlag opens a file only to read and sync it.
const existingSyncFlag = os.O_RDONLY

// readOnlyRefusal is false: a file is synced through a read-only handle here.
func readOnlyRefusal(*os.Root, string, error) bool { return false }

func replaceFile(root *os.Root, from, to string) error { return root.Rename(from, to) }

// linkFile is root.Link, replaced by tests to fail.
var linkFile = func(root *os.Root, from, to string) error { return root.Link(from, to) }

// placeNew links to at from; it fails with fs.ErrExist when to exists. On a filesystem without
// hard links it creates to exclusively instead and copies from into it, synced (copyNew).
func placeNew(root *os.Root, from, to string) error {
	err := linkFile(root, from, to)
	if noHardLinks(err) {
		return copyNew(root, from, to)
	}
	return err
}

// noHardLinks is a link the filesystem refuses as such (EPERM, EXDEV, ENOTSUP or EOPNOTSUPP),
// not one that failed.
func noHardLinks(err error) bool {
	for _, e := range []error{syscall.EPERM, syscall.EXDEV, syscall.ENOTSUP, syscall.EOPNOTSUPP} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// copyNew creates to, failing with fs.ErrExist when it exists, and copies from into it with from's
// mode and modification time, then syncs it. A copy that fails part-way is removed: it is this
// call's own file.
func copyNew(root *os.Root, from, to string) (err error) {
	src, err := root.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	dst, err := root.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = root.Remove(to)
		}
	}()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if err := dst.Chmod(fi.Mode().Perm()); err != nil {
		return err
	}
	if err := root.Chtimes(to, fi.ModTime(), fi.ModTime()); err != nil {
		return err
	}
	return fsyncFile(dst)
}

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
