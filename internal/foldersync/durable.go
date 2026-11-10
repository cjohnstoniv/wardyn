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
	"os"
	"path"
	"path/filepath"
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
// it and syncs that directory. It returns the destination used. tmp is closed and removed in every
// case.
func installDurable(root *os.Root, tmp *os.File, tmpName string, mode InstallMode, dsts ...string) (string, error) {
	if err := syncTemp(tmp); err != nil {
		_ = tmp.Close()
		_ = root.Remove(tmpName)
		return "", err
	}
	return installSynced(root, tmp, tmpName, mode, dsts...)
}

// installSynced is installDurable after syncTemp: it closes tmp, then with InstallReplace renames it
// over dsts[0], and with InstallNew puts it at the first of dsts that is free, trying each on the
// same temp path and never replacing one; a destination already holding the same bytes counts as
// installed. The temp is removed after success or after the last name failed.
func installSynced(root *os.Root, tmp *os.File, tmpName string, mode InstallMode, dsts ...string) (string, error) {
	defer func() { _ = root.Remove(tmpName) }() // after a rename or move it names nothing
	if err := tmp.Close(); err != nil {
		return "", &InstallError{Step: "close", Err: err}
	}
	dst, step, err := place(root, tmpName, mode, dsts)
	if err != nil {
		return "", &InstallError{Step: step, Err: err}
	}
	if err := syncDirIn(root, path.Dir(dst)); err != nil {
		return dst, &InstallError{Step: "sync-dir", Installed: true, Err: err}
	}
	return dst, nil
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
// never written.
func keptAlready(root *os.Root, tmpName, dst string) (bool, error) {
	want, ok := fileSum(root, tmpName)
	if !ok {
		return false, nil
	}
	f, err := root.OpenFile(dst, existingSyncFlag, 0)
	if err != nil {
		return false, nil
	}
	defer f.Close()
	fi, ferr := f.Stat()
	li, lerr := root.Lstat(dst)
	if ferr != nil || lerr != nil || !fi.Mode().IsRegular() || !os.SameFile(fi, li) {
		return false, nil
	}
	h := sha256.New()
	if n, err := io.Copy(h, io.NewSectionReader(f, 0, fi.Size())); err != nil || n != fi.Size() || !bytes.Equal(h.Sum(nil), want) {
		return false, nil
	}
	return true, fsyncFile(f)
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

// mkdirAllDurable is root.MkdirAll that also syncs the parent of every directory it creates.
func mkdirAllDurable(root *os.Root, dir string) error {
	var made []string
	for p := dir; p != "." && p != "/"; p = path.Dir(p) {
		if _, err := root.Lstat(p); err == nil {
			break
		}
		made = append(made, p)
	}
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, p := range made {
		if err := syncDirIn(root, path.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}

// mkdirAllDurableOS is mkdirAllDurable for a directory named by its own path.
func mkdirAllDurableOS(dir string, perm fs.FileMode) error {
	var made []string
	for p := dir; filepath.Dir(p) != p; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			break
		}
		made = append(made, p)
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	for _, p := range made {
		if err := syncDirPath(filepath.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}
