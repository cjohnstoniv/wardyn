// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// Every laptop path a pass lists, reads or writes is opened here one directory at a time from the
// pass's root, never by a whole path handed to os.Root. os.Root keeps a path inside its root, but it
// follows a link that stays inside: a directory replaced by a link to a credential folder inside the
// chosen folder, after a walk or check saw a plain directory, would be read or written through.
// Here every directory on the way must be a plain directory, the handle opened must be the directory
// that was looked at, and only a path's last name is acted on, inside its parent's handle.

// ErrLinkInPath is a path with a link, or something else that is not a plain directory, where a
// directory belongs, or an entry replaced while it was opened.
var ErrLinkInPath = errors.New("a link, or an entry that is not a plain directory, is in the path")

// OpenDir opens the directory rel (slash-separated, relative to root; "." is root) without following
// a link at any component. The caller closes it.
func OpenDir(root *os.Root, rel string) (*os.Root, error) {
	if rel == "." {
		return root.OpenRoot(".")
	}
	cur := root
	for _, name := range strings.Split(rel, "/") {
		next, err := openChild(cur, name)
		closeIn(root, cur)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// openChild opens the directory name inside dir: a plain directory when looked at, and the handle
// opened is that same directory (os.Root would follow a link swapped in between the two).
func openChild(dir *os.Root, name string) (*os.Root, error) {
	if !plainName(name) {
		return nil, &os.PathError{Op: "open", Path: name, Err: ErrLinkInPath}
	}
	li, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if li.Mode().Type() != fs.ModeDir {
		return nil, &os.PathError{Op: "open", Path: name, Err: ErrLinkInPath}
	}
	child, err := dir.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	if fi, err := child.Stat("."); err != nil || !os.SameFile(li, fi) {
		_ = child.Close()
		return nil, &os.PathError{Op: "open", Path: name, Err: ErrLinkInPath}
	}
	return child, nil
}

// plainName is one path component: not empty, not . or .., and holding no separator of this system.
func plainName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsFunc(name, func(r rune) bool { return r < 0x80 && os.IsPathSeparator(byte(r)) })
}

// inDir is name's parent directory, opened by OpenDir, and name's last component. closeIn closes it.
func inDir(root *os.Root, name string) (*os.Root, string, error) {
	parent, leaf := path.Split(name)
	if !plainName(leaf) && (leaf != "." || parent != "") {
		return nil, "", &os.PathError{Op: "open", Path: name, Err: ErrLinkInPath}
	}
	if parent == "" {
		return root, leaf, nil
	}
	dir, err := OpenDir(root, strings.TrimSuffix(parent, "/"))
	return dir, leaf, err
}

// closeIn closes dir unless it is root, which its caller owns.
func closeIn(root, dir *os.Root) {
	if dir != root {
		_ = dir.Close()
	}
}

// openIn is root.OpenFile with no link followed above name or at it: the file opened must be the
// entry its directory holds under that name.
func openIn(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	dir, leaf, err := inDir(root, name)
	if err != nil {
		return nil, err
	}
	defer closeIn(root, dir)
	f, err := dir.OpenFile(leaf, flag, perm)
	if err != nil {
		return nil, err
	}
	fi, ferr := f.Stat()
	li, lerr := dir.Lstat(leaf)
	if ferr != nil || lerr != nil || !os.SameFile(fi, li) {
		_ = f.Close()
		return nil, &os.PathError{Op: "open", Path: name, Err: ErrLinkInPath}
	}
	return f, nil
}

// readFileIn is root.ReadFile through openIn.
func readFileIn(root *os.Root, name string) ([]byte, error) {
	f, err := openIn(root, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// lstatIn, removeIn and chtimesIn act on name's last component inside its parent's handle.
func lstatIn(root *os.Root, name string) (os.FileInfo, error) {
	dir, leaf, err := inDir(root, name)
	if err != nil {
		return nil, err
	}
	defer closeIn(root, dir)
	return dir.Lstat(leaf)
}

func removeIn(root *os.Root, name string) error {
	dir, leaf, err := inDir(root, name)
	if err != nil {
		return err
	}
	defer closeIn(root, dir)
	return dir.Remove(leaf)
}

func chtimesIn(root *os.Root, name string, atime, mtime time.Time) error {
	dir, leaf, err := inDir(root, name)
	if err != nil {
		return err
	}
	defer closeIn(root, dir)
	return dir.Chtimes(leaf, atime, mtime)
}

// renameIn and linkIn act inside one directory, as every install here does (its temp is made beside
// its destination); two names in different directories are refused.
func renameIn(root *os.Root, from, to string) error {
	return inOneDir(root, from, to, (*os.Root).Rename)
}

func linkIn(root *os.Root, from, to string) error { return inOneDir(root, from, to, (*os.Root).Link) }

func inOneDir(root *os.Root, from, to string, op func(*os.Root, string, string) error) error {
	if path.Dir(from) != path.Dir(to) || !plainName(path.Base(to)) {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: ErrLinkInPath}
	}
	dir, leaf, err := inDir(root, from)
	if err != nil {
		return err
	}
	defer closeIn(root, dir)
	return op(dir, leaf, path.Base(to))
}

// mkdirAllIn is root.MkdirAll with no link followed: each directory on the way is made when missing
// and must then be a plain directory.
func mkdirAllIn(root *os.Root, name string, perm os.FileMode) error {
	if name == "." {
		return nil
	}
	cur := root
	defer func() { closeIn(root, cur) }()
	for _, c := range strings.Split(name, "/") {
		if !plainName(c) {
			return &os.PathError{Op: "mkdir", Path: name, Err: ErrLinkInPath}
		}
		if err := cur.Mkdir(c, perm); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		next, err := openChild(cur, c)
		if err != nil {
			return err
		}
		closeIn(root, cur)
		cur = next
	}
	return nil
}

// FS is root as an fs.FS for fs.WalkDir that follows no link: each directory it lists is opened by
// OpenDir and each file by openIn.
func FS(root *os.Root) fs.FS { return linkFreeFS{root} }

type linkFreeFS struct{ root *os.Root }

func (l linkFreeFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return openIn(l.root, name, os.O_RDONLY, 0)
}

// ReadDir lists the directory name sorted by file name, as fs.ReadDirFS requires.
func (l linkFreeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	dir, err := OpenDir(l.root, name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	d, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	slices.SortFunc(ents, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return ents, err
}
