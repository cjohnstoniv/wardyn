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
)

// Digest is a directory tree's identity: how many entries it holds and the sha256 over its sorted lines.
type Digest struct {
	Entries int    `json:"entries"`
	SHA256  string `json:"tree_sha256"`
}

// TreeDigest digests the tree under dir. Each entry contributes one line,
// "path TAB type TAB perm TAB size TAB sha256", sorted bytewise by path. The type is f, d or l; a symlink's
// sha256 is over its target string and it is never followed; perm is the octal permission bits. Owner,
// group and times are left out, because a restore onto another host changes them. Any other file type
// (socket, device, FIFO) fails the digest, naming the path. Reads go through os.Root, so no entry
// resolves outside dir.
func TreeDigest(dir string) (Digest, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Digest{}, err
	}
	defer root.Close()

	type line struct{ path, text string }
	var lines []line
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
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
		lines = append(lines, line{path, fmt.Sprintf("%s\t%s\t%04o\t%d\t%s\n", strconv.Quote(path), typ, info.Mode().Perm(), size, sum)})
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

func hashFile(root *os.Root, path string) (string, error) {
	f, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
