// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitpack

import (
	"runtime"
	"strings"
	"testing"
)

// allocatedBytes is the heap fn allocates, measured as the change in TotalAlloc.
func allocatedBytes(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A commit or tag whose headers are millions of short lines is read without a copy of the object or a
// slice of every line: the work is bounded by what the parser keeps, not by the line count, since one
// object may inflate to 32 MiB of two-byte lines and a pack of it compresses to a few KiB.
func TestCommitAndTagHeadersAreReadWithoutPerLineAllocation(t *testing.T) {
	oid := strings.Repeat("a", 40)
	dense := strings.Repeat("x\n", 2<<20) // 2 Mi header lines, 4 MiB
	commit := []byte("tree " + oid + "\n" + dense + "\nmessage\n")
	tag := []byte("object " + oid + "\n" + dense + "\nmessage\n")
	const bound = 64 << 10

	var err error
	if n := allocatedBytes(func() { _, err = parseCommit(commit, 20) }); n > bound {
		t.Errorf("parseCommit of a dense-header commit allocated %d bytes, want at most %d", n, bound)
	}
	if err != nil {
		t.Fatalf("parseCommit: %v", err)
	}
	var ok bool
	if n := allocatedBytes(func() { _, ok = tagTarget(tag, 20) }); n > bound {
		t.Errorf("tagTarget of a dense-header tag allocated %d bytes, want at most %d", n, bound)
	}
	if !ok {
		t.Fatal("tagTarget did not read the tag's object header")
	}
}
