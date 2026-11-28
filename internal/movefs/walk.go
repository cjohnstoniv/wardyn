// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package movefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// Export writes set's files under base as a movepack stream on out, and returns its manifest. The walk is
// depth first, each directory's children in byte order, which is the stream's order. It never follows a link:
// every path is read with Lstat, a symlink is carried as its text (a root that is one is refused by the
// writer), and a file is opened with O_NOFOLLOW and must still be the file Lstat saw. A hardlink is carried
// as a file. Fifos and devices are skipped and listed; sockets are left out. An excluded path is not
// descended into. A missing root is carried as nothing.
func Export(out io.Writer, set movepack.Set, base string) (movepack.Manifest, error) {
	w, err := movepack.NewWriter(out, set)
	if err != nil {
		return movepack.Manifest{}, err
	}
	wk := &walker{w: w, exclude: set.Exclude, base: base}
	roots := slices.Clone(set.Roots)
	slices.SortFunc(roots, compareStreamOrder)
	for _, root := range roots {
		rel := root[1:]
		fi, err := os.Lstat(wk.disk(rel))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return movepack.Manifest{}, fmt.Errorf("movefs: export %s: %w", root, err)
		}
		if err := wk.entry(rel, fi); err != nil {
			return movepack.Manifest{}, err
		}
	}
	return w.Close()
}

type walker struct {
	w       *movepack.Writer
	exclude []string
	base    string
}

// disk is where rel, a stream path, is on this filesystem.
func (wk *walker) disk(rel string) string { return filepath.Join(wk.base, "/"+rel) }

func (wk *walker) entry(rel string, fi fs.FileInfo) error {
	if movepack.Excluded(rel, wk.exclude) {
		return nil
	}
	m := fi.Mode()
	switch {
	case m.IsDir():
		return wk.dir(rel, fi)
	case m.IsRegular():
		return wk.file(rel, fi)
	case m&fs.ModeSymlink != 0:
		link, err := os.Readlink(wk.disk(rel))
		if err != nil {
			return fmt.Errorf("movefs: export: read link /%s: %w", rel, err)
		}
		return wk.w.Add(movepack.Header{Path: rel, Type: movepack.TypeSymlink, Mode: int64(m.Perm()), Link: link}, nil)
	case m&(fs.ModeNamedPipe|fs.ModeDevice|fs.ModeCharDevice) != 0:
		return wk.w.Skip(rel)
	case m&fs.ModeSocket != 0:
		// tar has no socket type: left out and not listed, as an engine's archive leaves it.
		return nil
	}
	return fmt.Errorf("%w: /%s is of a type a move does not carry", movepack.ErrArchiveRefused, rel)
}

func (wk *walker) dir(rel string, fi fs.FileInfo) error {
	if err := wk.w.Add(movepack.Header{Path: rel, Type: movepack.TypeDir, Mode: int64(fi.Mode().Perm())}, nil); err != nil {
		return err
	}
	parent, err := os.Lstat(wk.disk(path.Dir(rel)))
	if err != nil {
		return fmt.Errorf("movefs: export: stat the parent of /%s: %w", rel, err)
	}
	isMount := statOf(fi).Dev != statOf(parent).Dev
	ents, err := os.ReadDir(wk.disk(rel)) // sorted by name, in byte order
	if err != nil {
		return fmt.Errorf("movefs: export: read /%s: %w", rel, err)
	}
	for _, e := range ents {
		child := rel + "/" + e.Name()
		cfi, err := os.Lstat(wk.disk(child))
		if err != nil {
			return fmt.Errorf("movefs: export: stat /%s: %w", child, err)
		}
		if skipsRootLostFound(e.Name(), cfi.IsDir(), statOf(cfi).Uid, isMount) {
			continue
		}
		if err := wk.entry(child, cfi); err != nil {
			return err
		}
	}
	return nil
}

func (wk *walker) file(rel string, fi fs.FileInfo) error {
	f, err := os.OpenFile(wk.disk(rel), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("movefs: export: open /%s: %w", rel, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("movefs: export: stat /%s: %w", rel, err)
	}
	if !st.Mode().IsRegular() || !os.SameFile(fi, st) {
		return fmt.Errorf("movefs: export: /%s changed during the export", rel)
	}
	return wk.w.Add(movepack.Header{Path: rel, Type: movepack.TypeFile, Mode: int64(st.Mode().Perm()), Size: st.Size()}, f)
}

// statOf is fi's raw stat. Every FileInfo here comes from Lstat or Fstat on Linux.
func statOf(fi fs.FileInfo) *syscall.Stat_t { return fi.Sys().(*syscall.Stat_t) }
