// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import (
	"bytes"
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

// copyHandle is copyNew's copy in progress: the file it copies from, the copy's open handle, what the
// handle reported just after the create (nil if that read failed, or the name did not tell the same number then),
// and the bytes written to it.
type copyHandle struct {
	src, dst *os.File
	made     os.FileInfo
	written  int64
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
	c := &copyHandle{src: src, dst: dst}
	if made, merr := statCopy(dst); merr == nil {
		if li, lerr := lstatIn(root, to); lerr == nil && os.SameFile(made, li) {
			c.made = made // the handle and the name told the same number when the copy was made
		}
	}
	defer func() {
		if err != nil {
			err = dropCopy(root, to, c, err)
		}
		if cerr := dst.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("%w; its partial copy is left at %s", cerr, to)
		}
	}()
	if c.written, err = io.Copy(dst, src); err != nil {
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
	if held, _ := copyAt(root, to, c); !held {
		return fmt.Errorf("%s changed while it was copied: %w", to, fs.ErrExist)
	}
	return nil
}

// statCopy is fstat on a copy's open handle; replaced by tests. The proofs read it after the copy's last write: a
// filesystem may number a file anew once its bytes land, and on macOS exFAT a number read before the write did not
// match the copy's name after it. The read just after the create only tells whether the number held (copyAt).
var statCopy = func(f *os.File) (os.FileInfo, error) { return f.Stat() }

// copyAt reports whether name holds the copy c holds open, by two proofs: the same file as c's handle reports now,
// and the size written. gone is true when name is free; when the handle's file has no name left, as a copy has once
// a save is renamed over it; or when name holds another file, the handle reported the same file from its create to
// now, and the path copyNew created (name, when copyAt is given that path and not the aside) told that number
// too when the copy was made, so its number did not change under the write and the mismatch is real: each way
// name holds nothing of the copy's. When none holds, name may hold the copy and is named, never removed.
func copyAt(root *os.Root, name string, c *copyHandle) (held, gone bool) {
	li, lerr := lstatIn(root, name)
	if errors.Is(lerr, fs.ErrNotExist) {
		return false, true
	}
	own, serr := statCopy(c.dst)
	if lerr != nil || serr != nil {
		return false, false
	}
	if os.SameFile(own, li) {
		return li.Size() == c.written, false
	}
	if c.made != nil && os.SameFile(c.made, own) {
		return false, true // the copy's number held through its write, so the file at name is not the copy
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

// dropCopy removes copyNew's partial copy c after err, and returns err. It removes no name it has not proved holds
// the copy (copyAt): while to holds it, to is moved aside in one step, onto an empty temp of its own, and the aside
// is removed only if it still holds the copy and the bytes the copy wrote (holdsCopyBytes). A file saved over to
// before that look stays at to. One saved after it is moved with the name, is kept at the aside, and the error
// names it, as is a copy someone rewrote in place. An empty copy is never moved or removed: its size proves
// nothing, and a filesystem may number empty files alike. A file that cannot be proved to be the copy, or moved
// aside, stays at to, and the error says only that it is kept there: nothing at that name is called the copy's,
// since it may be a save someone made in place.
func dropCopy(root *os.Root, to string, c *copyHandle, err error) error {
	if held, gone := copyAt(root, to, c); gone {
		return err
	} else if !held || c.written == 0 {
		return fmt.Errorf("%w; the file is kept at %s", err, to)
	}
	dropLooked()
	aside, f, terr := createTemp(root, path.Dir(to), ".wardyn-sync-", "")
	if terr != nil {
		return fmt.Errorf("%w; the file is kept at %s", err, to)
	}
	_ = f.Close()
	if replaceFile(root, to, aside) != nil {
		_ = removeIn(root, aside)
		return fmt.Errorf("%w; the file is kept at %s", err, to)
	}
	if held, _ := copyAt(root, aside, c); !held || !holdsCopyBytes(root, aside, c) {
		return fmt.Errorf("%w; %w", err, keptAt(aside))
	}
	_ = removeIn(root, aside)
	return err
}

// holdsCopyBytes reports whether name holds exactly the bytes c wrote, the first c.written bytes of c.src: the last
// proof before a remove, since a rewrite in place through the name keeps the file and can keep its size.
func holdsCopyBytes(root *os.Root, name string, c *copyHandle) bool {
	want, ok := sumOf(c.src, c.written)
	if !ok {
		return false
	}
	f, err := openIn(root, name, os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() != c.written {
		return false
	}
	got, ok := sumOf(f, c.written)
	return ok && bytes.Equal(got, want)
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
