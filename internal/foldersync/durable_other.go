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

func replaceFile(root *os.Root, from, to string) error { return renameIn(root, from, to) }

// linkFile is root.Link, replaced by tests to fail.
var linkFile = func(root *os.Root, from, to string) error { return linkIn(root, from, to) }

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
// only if to still names it after the sync (copyAt): a file saved at to meanwhile, as an editor
// renames its save over the name, is never stamped or taken for the copy, and is fs.ErrExist. A copy
// that fails part-way is removed only where dropCopy proves the entry it removes is this copy, and is
// otherwise named. Its handle stays open until then: no other file is given an open file's inode
// number, so an entry that matches it is this copy and not a later save. A close that fails after the
// sync leaves the copy at to, and the error says so.
func copyNew(root *os.Root, from, to string) (err error) {
	src, err := openIn(root, from, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	dst, err := openIn(root, to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	var written int64
	defer func() {
		if err != nil {
			err = dropCopy(root, to, dst, written, err)
		}
		if cerr := dst.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("%w; its partial copy is left at %s", cerr, to)
		}
	}()
	if written, err = io.Copy(dst, src); err != nil {
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
	if held, _ := copyAt(root, to, dst, written); !held {
		return fmt.Errorf("%s changed while it was copied: %w", to, fs.ErrExist)
	}
	return nil
}

// statCopy is fstat on a copy's open handle; replaced by tests. It is read after the copy's last
// write, never before: a filesystem may number a file anew once its bytes land, and on macOS exFAT a
// number read before the write did not match the copy's name after it.
var statCopy = func(f *os.File) (os.FileInfo, error) { return f.Stat() }

// copyAt reports whether name holds the copy open at dst, by two proofs: the same file as dst's handle
// reports now, and the size written. gone is true when name is free, or when the handle's file has
// no name left, as a copy has once a save is renamed over it: either way name holds nothing of the
// copy's. When neither holds, name may hold the copy and is named, never removed.
func copyAt(root *os.Root, name string, dst *os.File, written int64) (held, gone bool) {
	li, lerr := lstatIn(root, name)
	if errors.Is(lerr, fs.ErrNotExist) {
		return false, true
	}
	own, serr := statCopy(dst)
	if lerr != nil || serr != nil {
		return false, false
	}
	if os.SameFile(own, li) {
		return li.Size() == written, false
	}
	st, ok := own.Sys().(*syscall.Stat_t)
	return false, ok && st.Nlink == 0
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

// dropCopy removes copyNew's partial copy, open at dst with written bytes, after err, and returns
// err. It removes no name it has not proved holds the copy (copyAt): while to holds it, to is moved
// aside in one step, onto an empty temp of its own, and the aside is removed only if it holds the
// copy. A file saved over to before that look stays at to. One saved after it is moved with the
// name, is kept at the aside, and the error names it. A copy that cannot be proved or moved aside
// stays at to, and the error says so.
func dropCopy(root *os.Root, to string, dst *os.File, written int64, err error) error {
	if held, gone := copyAt(root, to, dst, written); gone {
		return err
	} else if !held {
		return fmt.Errorf("%w; its partial copy is left at %s", err, to)
	}
	dropLooked()
	aside, f, terr := createTemp(root, path.Dir(to), ".wardyn-sync-", "")
	if terr != nil {
		return fmt.Errorf("%w; its partial copy is left at %s", err, to)
	}
	_ = f.Close()
	if replaceFile(root, to, aside) != nil {
		_ = removeIn(root, aside)
		return fmt.Errorf("%w; its partial copy is left at %s", err, to)
	}
	if held, _ := copyAt(root, aside, dst, written); !held {
		return fmt.Errorf("%w; %w", err, keptAt(aside))
	}
	_ = removeIn(root, aside)
	return err
}

func syncDirIn(root *os.Root, note *dirNote, dir string) error {
	d, err := openIn(root, dir, os.O_RDONLY, 0)
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
