// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package recoveryset holds the tree digest the recovery-set manifest records for a staged directory
// (docs/design/0.9/recovery-set.md, "Tree digest").
package recoveryset

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Digest is a directory tree's identity: how many entries it holds and the sha256 over its sorted lines.
type Digest struct {
	Entries int    `json:"entries"`
	SHA256  string `json:"tree_sha256"`
}

// TreeDigest digests the tree under dir. Each entry contributes one line,
// "path TAB type TAB perm TAB size TAB sha256", sorted bytewise by path. The type is f, d or l; a symlink's
// sha256 is over its target string and it is never followed; perm is the four-digit octal mode, the
// permission bits with the setuid (4), setgid (2) and sticky (1) bits in the leading digit. Owner,
// group and times are left out, because a restore onto another host changes them. Any other file type
// (socket, device, FIFO) fails the digest, naming the path. Reads go through os.Root, so no entry
// resolves outside dir.
func TreeDigest(dir string) (Digest, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Digest{}, err
	}
	defer root.Close()
	return digestRoot(root)
}

// TreeDigestIn is TreeDigest of the directory rel inside root. rel and every directory on the way to it
// resolve inside root, so a symlink leading out of it is an error, not a read of whatever it points at.
func TreeDigestIn(root *os.Root, rel string) (Digest, error) {
	sub, err := root.OpenRoot(rel)
	if err != nil {
		return Digest{}, err
	}
	defer sub.Close()
	return digestRoot(sub)
}

func digestRoot(root *os.Root) (Digest, error) {

	type line struct{ path, text string }
	var lines []line
	err := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		typ, size, sum := "", info.Size(), ""
		switch mode := info.Mode(); {
		case mode.IsDir():
			typ, size = "d", 0
		case mode.IsRegular():
			typ = "f"
			if sum, err = hashFile(root, path); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			target, err := root.Readlink(path)
			if err != nil {
				return err
			}
			typ, size = "l", int64(len(target))
			s := sha256.Sum256([]byte(target))
			sum = hex.EncodeToString(s[:])
		default:
			return fmt.Errorf("%s is a %s, which a recovery set cannot hold", strconv.Quote(path), strings.ToLower(mode.Type().String()))
		}
		lines = append(lines, line{path, fmt.Sprintf("%s\t%s\t%04o\t%d\t%s\n", strconv.Quote(path), typ, unixMode(info.Mode()), size, sum)})
		return nil
	})
	if err != nil {
		return Digest{}, err
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].path < lines[j].path })
	h := sha256.New()
	for _, l := range lines {
		_, _ = io.WriteString(h, l.text)
	}
	return Digest{Entries: len(lines), SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// unixMode is the permission bits with setuid, setgid and sticky in the leading octal digit.
func unixMode(m fs.FileMode) uint32 {
	out := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		out |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		out |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		out |= 0o1000
	}
	return out
}

func hashFile(root *os.Root, path string) (string, error) {
	// O_NONBLOCK so a file swapped for a FIFO after the type check cannot hang the open.
	f, err := root.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", strconv.Quote(path))
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
