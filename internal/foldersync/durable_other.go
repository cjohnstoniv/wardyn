// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"syscall"
	"time"
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
// mode and modification time, both set through its handle, then syncs it. The copy counts as placed
// only if to still names it after the sync: a file saved at to meanwhile, as an editor renames its
// save over the name, is never stamped or taken for the copy, and is fs.ErrExist. A copy that fails
// part-way is removed only where dropCopy is sure the entry it removes is this copy. Its handle stays
// open until then: no other file is given an open file's inode number, so an entry that matches
// own is this copy and not a later save. A close that fails after the sync leaves the copy at to,
// and the error says so.
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
	own, err := dst.Stat()
	if err != nil {
		_ = dst.Close()
		return fmt.Errorf("%w; its partial copy is left at %s", err, to)
	}
	defer func() {
		if err != nil {
			err = dropCopy(root, to, own, err)
		}
		if cerr := dst.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("%w; its partial copy is left at %s", cerr, to)
		}
	}()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if err := dst.Chmod(fi.Mode().Perm()); err != nil {
		return err
	}
	if err := setModTime(dst, fi.ModTime()); err != nil {
		return err
	}
	if err := fsyncFile(dst); err != nil {
		return err
	}
	if li, err := root.Lstat(to); err != nil || !os.SameFile(own, li) {
		return fmt.Errorf("%s changed while it was copied: %w", to, fs.ErrExist)
	}
	return nil
}

// setModTime sets f's access and modification times to t through its handle (futimes), so they land
// on this file whatever has been renamed over its name since; to the microsecond, which is all
// futimes takes. Replaced by tests.
var setModTime = func(f *os.File, t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Futimes(int(f.Fd()), []syscall.Timeval{tv, tv})
}

// dropLooked runs in dropCopy after its look at the copy's name and before its move; tests save there.
var dropLooked = func() {}

// dropCopy removes copyNew's partial copy own after err, and returns err; own's handle is still
// open. It removes no name it has not seen hold own: while to still names own, to is moved aside in
// one step, onto an empty temp of its own, and the aside is removed only if it holds own. A file
// saved over to before that look stays at to. One saved after it is moved with the name, is kept at
// the aside, and the error names it. A copy that cannot be moved aside stays at to, and the error
// says so.
func dropCopy(root *os.Root, to string, own os.FileInfo, err error) error {
	if li, lerr := root.Lstat(to); lerr != nil || !os.SameFile(own, li) {
		return err
	}
	dropLooked()
	aside, f, terr := createTemp(root, path.Dir(to), ".wardyn-sync-", "")
	if terr != nil {
		return fmt.Errorf("%w; its partial copy is left at %s", err, to)
	}
	_ = f.Close()
	if replaceFile(root, to, aside) != nil {
		_ = root.Remove(aside)
		return fmt.Errorf("%w; its partial copy is left at %s", err, to)
	}
	if li, lerr := root.Lstat(aside); lerr != nil || !os.SameFile(own, li) {
		return fmt.Errorf("%w; %w", err, keptAt(aside))
	}
	_ = root.Remove(aside)
	return err
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
		note.unsupported(reasonDirSync, dir, err)
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
