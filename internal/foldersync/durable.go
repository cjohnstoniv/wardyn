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

// InstallError is a failed install. Installed is set only when the new file is in place but its
// directory sync failed (errors.Is(err, ErrNotDurable)); otherwise every destination is as it was.
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
		return &InstallError{Step: "sync the file", Err: err}
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

// installSynced is installDurable after syncTemp. With InstallReplace it closes tmp and renames it
// over dsts[0]. With InstallNew it links the still-open tmp at the first of dsts that is free, never
// replacing one; a destination already holding tmp's bytes counts as installed.
func installSynced(root *os.Root, tmp *os.File, tmpName string, mode InstallMode, dsts ...string) (string, error) {
	dst, err := place(root, tmp, tmpName, mode, dsts)
	_ = tmp.Close()
	_ = root.Remove(tmpName)
	if err != nil {
		return "", &InstallError{Step: "install", Err: err}
	}
	if err := syncDirIn(root, path.Dir(dst)); err != nil {
		return dst, &InstallError{Step: "sync the directory", Installed: true, Err: err}
	}
	return dst, nil
}

func place(root *os.Root, tmp *os.File, tmpName string, mode InstallMode, dsts []string) (string, error) {
	if mode == InstallReplace {
		if err := tmp.Close(); err != nil {
			return "", err
		}
		return dsts[0], replaceFile(root, tmpName, dsts[0])
	}
	for _, dst := range dsts {
		err := linkNew(root, tmpName, dst)
		if err == nil {
			return dst, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		if sameContent(root, tmp, dst) {
			return dst, nil
		}
	}
	return "", errNamesTaken
}

// sameContent reports whether the regular file dst holds tmp's bytes.
func sameContent(root *os.Root, tmp *os.File, dst string) bool {
	fi, err := root.Lstat(dst)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	ti, err := tmp.Stat()
	if err != nil || ti.Size() != fi.Size() {
		return false
	}
	f, err := root.Open(dst)
	if err != nil {
		return false
	}
	defer f.Close()
	a, b := sha256.New(), sha256.New()
	_, aerr := io.Copy(a, io.NewSectionReader(tmp, 0, ti.Size()))
	_, berr := io.Copy(b, io.LimitReader(f, fi.Size()))
	return aerr == nil && berr == nil && bytes.Equal(a.Sum(nil), b.Sum(nil))
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
