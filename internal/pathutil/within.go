// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package pathutil is the path checks the packages that bound a path by a root share: containment (Within), so they
// cannot disagree on a root of "/", a trailing slash or a ".." element, and depth under a walk's root (DepthUnder).
package pathutil

import (
	"path/filepath"
	"strings"
)

// Within reports whether p is root or lies under it. Both are cleaned and compared element by element
// (filepath.Rel): /a/b is within /a and /ab is not, every absolute path is within "/", and /a/../b is not within /a.
// An absolute and a relative path are never within each other, and two relative paths compare as written. It reads
// no filesystem: a caller that must follow symlinks resolves p first.
func Within(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// DepthUnder returns how many segments the slash-separated relative path rel lies below its walk's root: 0 for ".",
// 1 for "a", 2 for "a/b". rel is as fs.WalkDir yields it, cleaned and with no leading or trailing slash.
func DepthUnder(rel string) int {
	if rel == "." {
		return 0
	}
	return strings.Count(rel, "/") + 1
}
