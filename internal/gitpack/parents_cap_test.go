// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitpack

import (
	"errors"
	"strings"
	"testing"
)

// commitWithParents is a commit object with n parent headers.
func commitWithParents(n int) []byte {
	oid := strings.Repeat("a", 40)
	return []byte("tree " + oid + "\n" + strings.Repeat("parent "+oid+"\n", n) + "\nmessage\n")
}

// A commit may name up to maxCommitParents parents. One more is refused as an inspection ceiling, with
// fixed text, before the parser keeps more: a 32 MiB commit of parent lines (about 700,000) would
// otherwise keep a string for every one.
func TestCommitParentCountIsCapped(t *testing.T) {
	c, err := parseCommit(commitWithParents(maxCommitParents), 20)
	if err != nil || len(c.parents) != maxCommitParents {
		t.Fatalf("a commit with %d parents: %d parents, %v", maxCommitParents, len(c.parents), err)
	}
	_, err = parseCommit(commitWithParents(maxCommitParents+1), 20)
	if !errors.Is(err, ErrTooManyParents) || !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a commit with %d parents: %v, want ErrTooManyParents", maxCommitParents+1, err)
	}
	if want := "gitpack: the push exceeds an inspection ceiling: a commit with more than 1024 parents"; err.Error() != want {
		t.Errorf("refusal = %q, want %q", err, want)
	}

	huge := commitWithParents((maxObjectBytes - 64) / len("parent "+strings.Repeat("a", 40)+"\n"))
	const bound = 256 << 10
	if n := allocatedBytes(func() { _, err = parseCommit(huge, 20) }); n > bound {
		t.Errorf("parseCommit of a %d-byte commit of parent lines allocated %d bytes, want at most %d", len(huge), n, bound)
	}
	if !errors.Is(err, ErrTooManyParents) {
		t.Fatalf("the 32 MiB commit: %v, want ErrTooManyParents", err)
	}
}
