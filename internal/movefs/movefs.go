// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package movefs reads a stopped sandbox's carried files into a movepack stream, and writes one beneath fresh
// mount roots. It runs inside a move's helper pod, as the proxy binary's -move-export and -move-import, where
// the claims a move carries are mounted at the paths the sandbox saw them at.
package movefs

import (
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// ExtractOptions is how Extract writes a stream.
type ExtractOptions struct {
	// Set is what the move carries; the reader refuses an entry outside it.
	Set movepack.Set
	// MaxBytes is the move's own bound on the stream. A missing or zero one is refused.
	MaxBytes int64
	// Mounts are the absolute sandbox paths the move's claims are mounted at. Every entry is written beneath
	// the one it is at or under; an entry under none is refused.
	Mounts []string
	// Base is the directory the sandbox's "/" is at: "/" in the helper, a temporary directory in a test.
	Base string
}

// Symlinks are held back until the stream's manifest has matched. Each one costs its path and target plus
// heldLinkCost, its record's own size with room for the slice's growth.
const (
	maxHeldLinkBytes = 64 << 20
	heldLinkCost     = 64
)

// skipsRootLostFound reports whether an entry is a filesystem's lost+found directly under its mount root: a
// directory of that name owned by uid 0. It is the filesystem's own, and the agent cannot read it.
func skipsRootLostFound(name string, isDir bool, uid uint32, parentIsMount bool) bool {
	return parentIsMount && isDir && uid == 0 && name == "lost+found"
}

// compareStreamOrder orders absolute paths as the stream does, a component at a time: "/" maps below every
// byte.
func compareStreamOrder(a, b string) int {
	return strings.Compare(strings.ReplaceAll(a, "/", "\x00"), strings.ReplaceAll(b, "/", "\x00"))
}

// under reports whether p, a stream path, is root or beneath it.
func under(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }
