// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path"
	"slices"
	"strings"
)

// The ignore files `wardyn sync` reads in every directory of the laptop's tree,
// in this order: a .wardynignore line comes later than a .gitignore one, so its
// "!pattern" can bring a git-ignored file back into the sync.
var syncIgnoreFiles = []string{".gitignore", ".wardynignore"}

// maxSyncIgnoreFile bounds one ignore file; a bigger one is refused and read as empty.
const maxSyncIgnoreFile = 1 << 20

// syncPattern is one line of an ignore file, in git's syntax (gitignore(5)):
// "#" comments, "!" negation, a trailing "/" for directories only, a "/" at the
// start or in the middle anchors the pattern to its file's directory, and "**"
// matches any number of directories. Matched case- and normalisation-
// insensitively (syncFold), as the denylist is.
type syncPattern struct {
	segs     []string
	negate   bool
	dirOnly  bool
	anchored bool
}

func parseSyncPatterns(r io.Reader) []syncPattern {
	var out []syncPattern
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxSyncIgnoreFile)
	for sc.Scan() {
		if p, ok := parseSyncPattern(sc.Text()); ok {
			out = append(out, p)
		}
	}
	return out
}

func parseSyncPattern(line string) (syncPattern, bool) {
	line = strings.TrimSuffix(line, "\r")
	if !strings.HasSuffix(line, `\ `) {
		line = strings.TrimRight(line, " ")
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return syncPattern{}, false
	}
	var p syncPattern
	switch {
	case strings.HasPrefix(line, "!"):
		p.negate, line = true, line[1:]
	case strings.HasPrefix(line, `\!`), strings.HasPrefix(line, `\#`):
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly, line = true, strings.TrimRight(line, "/")
	}
	p.anchored = strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return syncPattern{}, false
	}
	p.segs = strings.Split(syncFold(line), "/")
	return p, true
}

// match reports whether rel (relative to the pattern's own directory) matches.
func (p syncPattern) match(rel string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	segs := strings.Split(syncFold(rel), "/")
	if !p.anchored {
		return globSeg(p.segs[0], segs[len(segs)-1])
	}
	return globSegs(p.segs, segs)
}

func globSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if globSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 || !globSeg(pat[0], segs[0]) {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

func globSeg(pat, name string) bool {
	ok, err := path.Match(pat, name)
	return err == nil && ok
}

// syncIgnore holds the ignore rules of one pass: the organisation's wall, then
// every ignore file read in the laptop's tree, keyed by the directory it sits in.
type syncIgnore struct {
	org   []syncPattern
	byDir map[string][]syncPattern
}

func newSyncIgnore(org []string) *syncIgnore {
	ig := &syncIgnore{byDir: map[string][]syncPattern{}}
	for _, line := range org {
		if p, ok := parseSyncPattern(line); ok {
			ig.org = append(ig.org, p)
		}
	}
	return ig
}

// load reads dir's ignore files from the laptop's root ("." is the root). A
// missing, oversized or non-regular one adds nothing.
func (ig *syncIgnore) load(root *os.Root, dir string) {
	key := dir
	if key == "." {
		key = ""
	}
	for _, name := range syncIgnoreFiles {
		rel := path.Join(dir, name)
		fi, err := root.Lstat(rel)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxSyncIgnoreFile {
			continue
		}
		b, err := root.ReadFile(rel)
		if err != nil {
			continue
		}
		ig.byDir[key] = append(ig.byDir[key], parseSyncPatterns(bytes.NewReader(b))...)
	}
}

// orgDenied reports a path the organisation's wall covers. No ignore file can
// take it back.
func (ig *syncIgnore) orgDenied(rel string, isDir bool) bool {
	matched := false
	for _, p := range ig.org {
		if p.match(rel, isDir) {
			matched = !p.negate
		}
	}
	return matched
}

// ignored reports a path the tree's ignore files leave out. The walks ask it on
// the way down, so a path under an ignored directory is never asked about.
// Rules are applied from the root's file to the deepest, each in file order,
// and the last one that matches decides, as git does.
func (ig *syncIgnore) ignored(rel string, isDir bool) bool {
	matched := false
	for dir, rest := "", rel; ; {
		for _, p := range ig.byDir[dir] {
			if p.match(rest, isDir) {
				matched = !p.negate
			}
		}
		seg, tail, more := strings.Cut(rest, "/")
		if !more {
			return matched
		}
		dir, rest = path.Join(dir, seg), tail
	}
}

// syncIsIgnoreFile reports an ignore file at any depth, by any spelling a
// case- or normalisation-insensitive laptop filesystem would open as one. The
// rules that bind the laptop are the laptop's own: the sandbox never writes,
// replaces or deletes one there.
func syncIsIgnoreFile(rel string) bool {
	base := syncFold(path.Base(rel))
	for _, name := range syncIgnoreFiles {
		if base == syncFold(name) {
			return true
		}
	}
	return false
}

// syncNamesIgnoreFile reports an ignore-file name at any component of rel, a directory
// included: creating `.wardynignore/` on the laptop would block the laptop's own file of that name.
func syncNamesIgnoreFile(rel string) bool {
	return slices.ContainsFunc(strings.Split(rel, "/"), syncIsIgnoreFile)
}
