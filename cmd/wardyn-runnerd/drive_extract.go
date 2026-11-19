// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"archive/tar"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
)

// `drive open` reads a drive's files from a throwaway guest an agent's disk may have compromised, so the
// stream is hostile: this file turns it into plain files and directories under one os.Root, and turns the
// person's changes back into a stream for the guest. Nothing here follows a link, makes one, or lets a
// name leave the root.

// What one copy may hold. A name is capped at what macOS itself accepts (PATH_MAX, NAME_MAX).
const (
	driveOpenMaxEntries = 250_000
	driveOpenMaxPath    = 1024
	driveOpenMaxName    = 255
)

// driveOpenLimits caps a copy: entries counts every header, links and devices included, and bytes the files'.
type driveOpenLimits struct {
	entries int
	bytes   int64
}

// The kinds of entry a copy leaves out, in the order the closing line lists them.
const (
	driveOpenSkipLink = iota
	driveOpenSkipDevice
	driveOpenSkipPipe
	driveOpenSkipSetID
	driveOpenSkipOther
	driveOpenSkipKinds
)

var driveOpenSkipWords = [driveOpenSkipKinds][2]string{
	{"link", "links"}, {"device", "devices"}, {"pipe", "pipes"}, {"set-id file", "set-id files"}, {"other entry", "other entries"},
}

// driveOpenSkips counts what a copy left out, by kind.
type driveOpenSkips [driveOpenSkipKinds]int

// text is "Skipped 3: 2 links, 1 device." and "" when nothing was skipped.
func (s driveOpenSkips) text() string {
	var parts []string
	total := 0
	for kind, n := range s {
		if n > 0 {
			total += n
			parts = append(parts, driveOpenCount(n, driveOpenSkipWords[kind][0], driveOpenSkipWords[kind][1]))
		}
	}
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("Skipped %d: %s.", total, strings.Join(parts, ", "))
}

func driveOpenCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// driveOpenSize is a byte count as a person reads it: "38.2 MB".
func driveOpenSize(n int64) string {
	f := float64(n)
	for _, unit := range []string{"B", "kB", "MB", "GB"} {
		if f < 1000 || unit == "GB" {
			if unit == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, unit)
		}
		f /= 1000
	}
	return ""
}

// driveOpenFile is a regular file as it was copied out: what write back compares against, and the mode
// the guest had, whose execute bit a rewrite keeps.
type driveOpenFile struct {
	sum  [sha256.Size]byte
	mode fs.FileMode
}

// driveOpenCopy is what an extraction left on this Mac.
type driveOpenCopy struct {
	files   int
	bytes   int64
	skipped driveOpenSkips
	base    map[string]driveOpenFile // slash-separated names under the root
	dirs    map[string]bool
	// unread and unreadText are what the guest's tar could not read: a count of its messages, and the first
	// few, escaped. The copy is then partial, and write back and delete know nothing of what is missing.
	unread     int
	unreadText []string
}

// driveOpenName is a tar entry's name as a path under the root, "" for the root itself. An absolute name,
// a ".." component, a NUL or a name past the limits is refused even though os.Root would stop it: the
// stream is hostile, and a listing that tries is not a listing worth keeping.
func driveOpenName(raw string) (string, error) {
	if raw == "" || strings.HasPrefix(raw, "/") || strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("the drive's listing has a name that is empty, absolute or has a NUL in it: %q", raw)
	}
	if len(raw) > driveOpenMaxPath {
		return "", fmt.Errorf("the drive's listing has a name longer than %d bytes", driveOpenMaxPath)
	}
	var keep []string
	for part := range strings.SplitSeq(raw, "/") {
		switch {
		case part == "..":
			return "", fmt.Errorf("the drive's listing has a name that leaves its folder: %q", raw)
		case len(part) > driveOpenMaxName:
			return "", fmt.Errorf("the drive's listing has a name part longer than %d bytes", driveOpenMaxName)
		case part != "" && part != ".":
			keep = append(keep, part)
		}
	}
	return strings.Join(keep, "/"), nil
}

// driveOpenKind is the skip kind of a tar entry that is not a file or a directory.
func driveOpenKind(typeflag byte) int {
	switch typeflag {
	case tar.TypeSymlink, tar.TypeLink:
		return driveOpenSkipLink
	case tar.TypeChar, tar.TypeBlock:
		return driveOpenSkipDevice
	case tar.TypeFifo:
		return driveOpenSkipPipe
	}
	return driveOpenSkipOther
}

// driveOpenExtract writes the files and directories of the tar stream r under root: regular files at mode
// 0644, never an execute bit, each passed to mark (the quarantine flag) before it is closed. Links,
// devices and pipes, and files with a set-id bit, are skipped and counted; a hostile name, a repeated
// one, more entries or bytes than lim allows, or a mark that fails ends the copy with an error, and the
// caller removes what was written.
func driveOpenExtract(r io.Reader, root *os.Root, lim driveOpenLimits, mark func(*os.File) error) (*driveOpenCopy, error) {
	c := &driveOpenCopy{base: map[string]driveOpenFile{}, dirs: map[string]bool{}}
	tr := tar.NewReader(r)
	for entries := 0; ; entries++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return c, nil
		}
		if err != nil {
			return nil, fmt.Errorf("the drive's listing is damaged: %w", err)
		}
		if entries >= lim.entries {
			return nil, fmt.Errorf("the drive holds more than %d files and folders; Wardyn copies no more than that", lim.entries)
		}
		name, err := driveOpenName(h.Name)
		if err != nil {
			return nil, err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := c.addDir(root, name, mark); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if h.Mode&(syscall.S_ISUID|syscall.S_ISGID) != 0 {
				c.skipped[driveOpenSkipSetID]++
				continue
			}
			if err := c.addFile(root, tr, h, name, lim, mark); err != nil {
				return nil, err
			}
		default:
			c.skipped[driveOpenKind(h.Typeflag)]++
		}
	}
}

// addDir makes a directory (and the ones above it), marks each new one, and notes it as copied.
func (c *driveOpenCopy) addDir(root *os.Root, name string, mark func(*os.File) error) error {
	if name == "" {
		return nil
	}
	if err := root.MkdirAll(name, 0o755); err != nil {
		return fmt.Errorf("make folder %q: %w", name, err)
	}
	parts := strings.Split(name, "/")
	for i := range parts {
		dir := strings.Join(parts[:i+1], "/")
		if c.dirs[dir] {
			continue
		}
		f, err := root.Open(dir)
		if err == nil {
			err = mark(f)
			_ = f.Close()
		}
		if err != nil {
			return fmt.Errorf("mark folder %q: %w", dir, err)
		}
		c.dirs[dir] = true
	}
	return nil
}

// addFile copies one regular file's bytes from tr, hashing them as they go.
func (c *driveOpenCopy) addFile(root *os.Root, tr *tar.Reader, h *tar.Header, name string, lim driveOpenLimits, mark func(*os.File) error) error {
	if name == "" {
		return errors.New("the drive's listing has a file with no name")
	}
	if h.Size < 0 || h.Size > lim.bytes-c.bytes {
		return fmt.Errorf("the drive's files are more than the %s this Mac has room for", driveOpenSize(lim.bytes))
	}
	if dir := path.Dir(name); dir != "." {
		if err := c.addDir(root, dir, mark); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("make file %q: %w", name, err)
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), tr)
	if err == nil && n != h.Size {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = mark(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("copy file %q: %w", name, err)
	}
	c.files++
	c.bytes += n
	was := driveOpenFile{mode: fs.FileMode(h.Mode & 0o777)}
	copy(was.sum[:], sum.Sum(nil))
	c.base[name] = was
	return nil
}

// driveOpenBack is what driveOpenPack found to write back. The files of the copy that changed (known) and
// the ones the copy did not hold (fresh, with new folders) go in two streams: the fresh one is extracted
// without overwriting, so a name the copy never held (an unreadable item, say) is never replaced.
type driveOpenBack struct {
	known, fresh []string // names of files in each stream
	newDirs      int
	skipped      driveOpenSkips
	gone         []string // copied files no longer there, sorted
}

func (b driveOpenBack) changed() int { return len(b.known) + len(b.fresh) }

// driveOpenPack writes to known and fresh, as tar streams for the guest, the files under root that differ
// from the copy, and those and the folders that the copy did not hold. It reads regular files only, with one
// link: a link, a device, a pipe or a set-id file is skipped and counted, and the walk never descends a
// link. gone lists the copied files that are missing now, for the delete flag; a name the copy never held is
// never in it.
func driveOpenPack(root *os.Root, c *driveOpenCopy, known, fresh io.Writer) (driveOpenBack, error) {
	var b driveOpenBack
	tk, tf := tar.NewWriter(known), tar.NewWriter(fresh)
	present := map[string]bool{}
	err := fs.WalkDir(root.FS(), ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		fi, err := root.Lstat(p)
		if err != nil {
			return err
		}
		present[p] = true
		switch mode := fi.Mode(); {
		case mode.IsDir():
			if c.dirs[p] {
				return nil
			}
			b.newDirs++
			return tf.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: p + "/", Mode: 0o755, ModTime: fi.ModTime()})
		case mode.IsRegular():
			return b.packFile(tk, tf, root, c, p, fi)
		case mode&fs.ModeSymlink != 0:
			b.skipped[driveOpenSkipLink]++
		case mode&fs.ModeDevice != 0:
			b.skipped[driveOpenSkipDevice]++
		case mode&fs.ModeNamedPipe != 0:
			b.skipped[driveOpenSkipPipe]++
		default:
			b.skipped[driveOpenSkipOther]++
		}
		return nil
	})
	if err == nil {
		err = errors.Join(tk.Close(), tf.Close())
	}
	for p := range c.base {
		if !present[p] {
			b.gone = append(b.gone, p)
		}
	}
	slices.Sort(b.gone)
	return b, err
}

// packFile adds the regular file p if it is new or changed. The file is opened through the root and must
// still be the file the walk saw, so a link swapped in meanwhile is not read.
func (b *driveOpenBack) packFile(tk, tf *tar.Writer, root *os.Root, c *driveOpenCopy, p string, fi fs.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		b.skipped[driveOpenSkipLink]++
		return nil
	}
	if fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
		b.skipped[driveOpenSkipSetID]++
		return nil
	}
	f, err := root.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	if now, err := f.Stat(); err != nil || !os.SameFile(fi, now) || !now.Mode().IsRegular() {
		b.skipped[driveOpenSkipLink]++
		return nil
	}
	sum := sha256.New()
	size, err := io.Copy(sum, f)
	if err != nil {
		return err
	}
	was, known := c.base[p]
	if known && string(sum.Sum(nil)) == string(was.sum[:]) {
		return nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	mode, tw := fs.FileMode(0o644), tf
	if known {
		mode, tw = was.mode, tk
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: p, Mode: int64(mode), Size: size, ModTime: fi.ModTime()}); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, f, size); err != nil {
		return err
	}
	if known {
		b.known = append(b.known, p)
	} else {
		b.fresh = append(b.fresh, p)
	}
	return nil
}
