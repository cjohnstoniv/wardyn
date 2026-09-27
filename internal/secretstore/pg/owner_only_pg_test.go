// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// TestPG_OwnRowOnlyNeverFallsBackToTheOperatorRow pins #1106 at the store: a
// read under GrantRead(ctx, true) matches only the view owner's own row, while
// the same read without it keeps the operator fallback, and each read reports
// whose row it opened.
func TestPG_OwnRowOnlyNeverFallsBackToTheOperatorRow(t *testing.T) {
	s, _, _ := newPGStore(t)
	ctx := context.Background()
	name := uniqueName("ado-pat")
	alice, bob := "alice-"+uuid.NewString(), "bob-"+uuid.NewString()
	t.Cleanup(func() {
		_ = s.Delete(ctx, name)
		_ = s.For(alice).Delete(ctx, name)
	})
	if err := s.Put(ctx, name, []byte("operator-v")); err != nil {
		t.Fatal(err)
	}
	if err := s.For(alice).Put(ctx, name, []byte("alice-v")); err != nil {
		t.Fatal(err)
	}

	read := func(owner string, ownOnly bool) ([]byte, string, error) {
		gctx, row := secretstore.GrantRead(ctx, ownOnly)
		v, err := s.For(owner).Get(gctx, name)
		return v, row.Scope(), err
	}

	if v, scope, err := read(bob, false); err != nil || string(v) != "operator-v" || scope != "operator" {
		t.Errorf("bob without owner_only = (%q, %q, %v), want the operator row (today's fallback)", v, scope, err)
	}
	if v, _, err := read(bob, true); !errors.Is(err, secretstore.ErrNotFound) {
		t.Errorf("bob with owner_only = (%q, %v), want not-found: the operator row must never serve an owner_only read", v, err)
	}
	if v, scope, err := read(alice, true); err != nil || string(v) != "alice-v" || scope != "own" {
		t.Errorf("alice with owner_only = (%q, %q, %v), want her own row", v, scope, err)
	}
	if v, _, err := read("", true); !errors.Is(err, secretstore.ErrNotFound) {
		t.Errorf("owner \"\" with owner_only = (%q, %v), want not-found: the operator namespace is never an owner_only read", v, err)
	}
}
