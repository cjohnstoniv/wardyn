// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package component

import (
	"slices"
	"testing"
)

type fakeCtor func() string

func TestRegistry_DefaultAndLookup(t *testing.T) {
	r := NewRegistry[fakeCtor]("a")
	r.Register("a", func() string { return "A" })
	r.Register("b", func() string { return "B" })

	// Empty name resolves to the default.
	c, resolved, err := r.Resolve("")
	if err != nil || resolved != "a" || c() != "A" {
		t.Fatalf("resolve default: %q %v", resolved, err)
	}
	c, resolved, err = r.Resolve("b")
	if err != nil || resolved != "b" || c() != "B" {
		t.Fatalf("resolve b: %q %v", resolved, err)
	}
	if _, _, err := r.Resolve("missing"); err == nil {
		t.Fatal("resolve of unregistered name must error")
	}
	if got := r.Names(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("Names = %v, want sorted [a b]", got)
	}
}

func TestRegistry_DuplicateAndEmptyPanic(t *testing.T) {
	r := NewRegistry[fakeCtor]("a")
	r.Register("a", func() string { return "A" })
	mustPanic(t, "duplicate", func() { r.Register("a", func() string { return "A2" }) })
	mustPanic(t, "empty name", func() { r.Register("", func() string { return "" }) })
}

func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic on %s", what)
		}
	}()
	fn()
}
