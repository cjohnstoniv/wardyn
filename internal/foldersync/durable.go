// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"sync"
)

// InstallMode says whether an install may replace what is at its destination.
type InstallMode int

const (
	// InstallReplace renames the temp file over the destination.
	InstallReplace InstallMode = iota
	// InstallNew links the temp file at the first free destination and never replaces one.
	InstallNew
)

// ErrNotDurable is an install that put the new file in place but could not prove it durable: the
// directory sync after the rename or link failed. Under InstallReplace the old file is gone.
var ErrNotDurable = errors.New("installed but not durable")

// errNamesTaken is an InstallNew whose every destination holds another file.
var errNamesTaken = fmt.Errorf("every name is taken: %w", fs.ErrExist)

// InstallError is a failed install. Before the install (Step sync-file, close, rename, link, or
// sync-existing: an identical file already at a new name could not be synced) every
// destination is as it was and the temp file is removed. After it (Step sync-dir, Installed set,
// errors.Is(err, ErrNotDurable)) the new content is visible at the destination, but not proven
// durable, and under InstallReplace the old content is gone.
type InstallError struct {
	Step      string
	Installed bool
	Err       error
}

func (e *InstallError) Error() string { return e.Step + ": " + e.Err.Error() }

func (e *InstallError) Unwrap() []error {
	if e.Installed {
		return []error{ErrNotDurable, e.Err}
	}
	return []error{e.Err}
}

// dirNote is one pass's record of a directory sync its filesystem does not offer (dirSyncUnsupported):
// best effort, so the install stands, but its folder entry may not survive a power loss. It is
// logged once per pass; a nil note logs each one.
// It also sets the pass's Report.DurabilityWarning.
type dirNote struct {
	folder string
	rep    *Report
	once   sync.Once
}

// dirSyncWarning is the warning of a dirNote.
const dirSyncWarning = "this folder's filesystem can't guarantee the folder entry survives a power loss"

func (n *dirNote) unsupported(dir string, err error) {
	log := func(folder string) {
		slog.Warn("foldersync: "+dirSyncWarning, slog.String("folder", folder), slog.String("dir", dir), slog.String("err", err.Error()))
	}
	if n == nil {
		log(dir)
		return
	}
	n.once.Do(func() {
		log(n.folder)
		if n.rep != nil {
			n.rep.DurabilityWarning = dirSyncWarning
		}
	})
}

// copyToLaptop is a pull's copy into its laptop temp file, replaced by tests to fail.
var copyToLaptop = func(dst *os.File, src io.Reader) (int64, error) { return io.Copy(dst, src) }

// The sync calls, replaced by tests to fail or record one.
var (
	fsyncFile = func(f *os.File) error { return f.Sync() }
	fsyncDir  = func(d *os.File) error { return d.Sync() }
)

// syncTemp is an install's first step: tmp's bytes reach the disk. It stays open.
func syncTemp(tmp *os.File) error {
	if err := fsyncFile(tmp); err != nil {
		return &InstallError{Step: "sync-file", Err: err}
	}
	return nil
}

// installDurable syncs tmp (named tmpName in root, in the directory of every destination), installs
// it and syncs that directory and every one above it in root. It returns the destination used. tmp is closed and removed in every
// case.
func installDurable(root *os.Root, note *dirNote, tmp *os.File, tmpName string, mode InstallMode, dsts ...string) (string, error) {
	if err := syncTemp(tmp); err != nil {
		_ = tmp.Close()
		_ = root.Remove(tmpName)
		return "", err
	}
	return installSynced(root, note, tmp, tmpName, mode, dsts...)
}

// installSynced is installDurable after syncTemp: it closes tmp, then with InstallReplace renames it
// over dsts[0], and with InstallNew puts it at the first of dsts that is free, trying each on the
// same temp path and never replacing one; a destination already holding the same bytes counts as
// installed. It then syncs the destination's directory and every one above it, through root, on
// every install: a retry, or a copy already in place, included. The temp is removed after success
// or after the last name failed.
func installSynced(root *os.Root, note *dirNote, tmp *os.File, tmpName string, mode InstallMode, dsts ...string) (string, error) {
	defer func() { _ = root.Remove(tmpName) }() // after a rename or move it names nothing
	if err := tmp.Close(); err != nil {
		return "", &InstallError{Step: "close", Err: err}
	}
	dst, step, err := place(root, tmpName, mode, dsts)
	if err != nil {
		return "", &InstallError{Step: step, Err: err}
	}
	if err := syncDirsUp(root, note, path.Dir(dst)); err != nil {
		return dst, &InstallError{Step: "sync-dir", Installed: true, Err: err}
	}
	return dst, nil
}

// syncDirsUp syncs dir and every directory above it in root, root itself last. An entry is durable
// only once every directory naming it is synced, and that a directory exists never says its own
// entry reached the disk: an earlier attempt may have made it and failed to sync its parent.
func syncDirsUp(root *os.Root, note *dirNote, dir string) error {
	for p := dir; ; p = path.Dir(p) {
		if err := syncDirIn(root, note, p); err != nil {
			return err
		}
		if p == "." {
			return nil
		}
	}
}

// place installs tmpName and names the step that failed.
func place(root *os.Root, tmpName string, mode InstallMode, dsts []string) (dst, step string, err error) {
	if mode == InstallReplace {
		return dsts[0], "rename", replaceFile(root, tmpName, dsts[0])
	}
	for _, dst := range dsts {
		err := placeNew(root, tmpName, dst)
		if err == nil {
			return dst, "", nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", "link", err
		}
		same, err := keptAlready(root, tmpName, dst)
		if err != nil {
			return "", "sync-existing", err
		}
		if same {
			return dst, "", nil
		}
	}
	return "", "link", errNamesTaken
}

// keptAlready reports whether the regular file dst holds tmpName's bytes, and when it does, syncs
// that same file: bytes only visible, from a write not yet on the disk, are not yet kept. dst is
// never written. Its directories are synced by the install that asked.
func keptAlready(root *os.Root, tmpName, dst string) (bool, error) {
	want, ok := fileSum(root, tmpName)
	if !ok {
		return false, nil
	}
	f, fi, err := openKept(root, dst)
	if err != nil {
		return false, nil
	}
	defer f.Close()
	if sum, ok := sumOf(f, fi.Size()); !ok || !bytes.Equal(sum, want) {
		return false, nil
	}
	return true, fsyncFile(f)
}

// errNotKeptFile is a laptop path that is not one regular file from its check to its open.
var errNotKeptFile = errors.New("not a regular file, or replaced while it was opened")

// openKept opens the laptop's regular file name in root, never through a link at its name: what
// is compared, synced and recorded is this one file, not one swapped in at its name.
func openKept(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	f, err := root.OpenFile(name, existingSyncFlag, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, ferr := f.Stat()
	li, lerr := root.Lstat(name)
	if ferr != nil || lerr != nil || !fi.Mode().IsRegular() || !os.SameFile(fi, li) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s: %w", name, errNotKeptFile)
	}
	return f, fi, nil
}

// sumOf is the sha256 of f's first size bytes, read from its start.
func sumOf(f *os.File, size int64) ([]byte, bool) {
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(f, 0, size))
	return h.Sum(nil), err == nil && n == size
}

// recordKept is the one place a laptop file is recorded as in step with the sandbox's side remote.
// It opens rel (openKept) and asks same whether this is the file the caller means; then it syncs
// that file, then its directory and every one above it, and only then records the side of the file
// it synced. A file that is not the one meant is (false, nil), with nothing recorded. A failed sync
// is an InstallError with Installed set, whatever its errno: only a directory sync the filesystem
// does not offer is passed over (syncDirIn).
func (sy *Syncer) recordKept(rel string, remote Side, same func(f *os.File, fi os.FileInfo) bool) (bool, error) {
	f, fi, err := openKept(sy.Root, rel)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if !same(f, fi) {
		return false, nil
	}
	if err := fsyncFile(f); err != nil {
		return false, &InstallError{Step: "sync-file", Installed: true, Err: err}
	}
	if err := syncDirsUp(sy.Root, sy.dirs, path.Dir(rel)); err != nil {
		return false, &InstallError{Step: "sync-dir", Installed: true, Err: err}
	}
	sy.State.Files[rel] = FileSeen{Local: localSide(fi), Remote: remote}
	return true, nil
}

// fileSum is the sha256 of the regular file name.
func fileSum(root *os.Root, name string) ([]byte, bool) {
	fi, err := root.Lstat(name)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, fi.Size()))
	return h.Sum(nil), err == nil && n == fi.Size()
}

// createTemp creates an exclusive 0600 file prefix<random>suffix in dir, open for reading and writing.
func createTemp(root *os.Root, dir, prefix, suffix string) (string, *os.File, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	name := path.Join(dir, prefix+hex.EncodeToString(b[:])+suffix)
	f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	return name, f, err
}
