// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A context carries its own CLIEnded, a bare one carries none, and a missing one counts nothing: the
// receipt of a call that put no counter on its context reads 0 and the cut still runs.
func TestCLIEndedOnAContext(t *testing.T) {
	if store.CLIEndedFrom(context.Background()) != nil {
		t.Fatal("a bare context carries a CLIEnded")
	}
	if n := store.CLIEndedFrom(context.Background()).Count(); n != 0 {
		t.Fatalf("a missing CLIEnded counted %d, want 0", n)
	}
	ctx, c := store.WithCLIEnded(context.Background())
	if store.CLIEndedFrom(ctx) != c || c.Count() != 0 {
		t.Fatalf("WithCLIEnded: from ctx = %p, made %p, count %d; want the same counter at 0", store.CLIEndedFrom(ctx), c, c.Count())
	}
	inner, c2 := store.WithCLIEnded(ctx)
	if store.CLIEndedFrom(inner) != c2 || c2 == c {
		t.Fatal("a nested WithCLIEnded did not give its own counter")
	}
}
