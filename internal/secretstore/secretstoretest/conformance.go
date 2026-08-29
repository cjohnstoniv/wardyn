// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretstoretest provides a reusable conformance suite for any
// secretstore.Store implementation. The blessed default (pg) and a future
// alternate (OpenBao/Vault/KMS) are held to the identical contract. It uses
// process-unique names so it is safe against a shared backing store.
package secretstoretest

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// RunConformance exercises the secretstore.Store contract. newStore must return
// a usable store on each call (it need not be empty; the suite uses unique names).
func RunConformance(t *testing.T, newStore func(t *testing.T) secretstore.Store) {
	ctx := context.Background()
	uniq := func(p string) string { return "conformance/" + p + "/" + uuid.NewString() }

	t.Run("put_get_roundtrip_binary", func(t *testing.T) {
		s := newStore(t)
		n := uniq("rt")
		val := []byte("binary\x00\xff\x00value")
		t.Cleanup(func() { _ = s.Delete(ctx, n) })
		if err := s.Put(ctx, n, val); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, err := s.Get(ctx, n)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !bytes.Equal(got, val) {
			t.Fatalf("round-trip mismatch: got %q want %q", got, val)
		}
	})

	t.Run("get_missing_returns_ErrNotFound", func(t *testing.T) {
		s := newStore(t)
		if _, err := s.Get(ctx, uniq("absent")); !errors.Is(err, secretstore.ErrNotFound) {
			t.Fatalf("Get(missing) err = %v, want secretstore.ErrNotFound", err)
		}
	})

	t.Run("overwrite_last_wins", func(t *testing.T) {
		s := newStore(t)
		n := uniq("rot")
		t.Cleanup(func() { _ = s.Delete(ctx, n) })
		_ = s.Put(ctx, n, []byte("v1"))
		if err := s.Put(ctx, n, []byte("v2")); err != nil {
			t.Fatalf("Put v2: %v", err)
		}
		got, _ := s.Get(ctx, n)
		if string(got) != "v2" {
			t.Fatalf("overwrite: got %q want v2", got)
		}
	})

	t.Run("delete_then_missing_idempotent", func(t *testing.T) {
		s := newStore(t)
		n := uniq("del")
		_ = s.Put(ctx, n, []byte("x"))
		if err := s.Delete(ctx, n); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := s.Delete(ctx, n); err != nil {
			t.Fatalf("second Delete must be idempotent: %v", err)
		}
		if _, err := s.Get(ctx, n); !errors.Is(err, secretstore.ErrNotFound) {
			t.Fatalf("Get(deleted) err = %v, want ErrNotFound", err)
		}
	})

	t.Run("list_contains_then_reflects_delete", func(t *testing.T) {
		s := newStore(t)
		keep, drop := uniq("keep"), uniq("drop")
		t.Cleanup(func() { _ = s.Delete(ctx, keep) })
		_ = s.Put(ctx, keep, []byte("k"))
		_ = s.Put(ctx, drop, []byte("d"))
		names, err := s.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if !slices.Contains(names, keep) || !slices.Contains(names, drop) {
			t.Fatalf("List missing our names")
		}
		_ = s.Delete(ctx, drop)
		names, _ = s.List(ctx)
		if slices.Contains(names, drop) {
			t.Fatalf("List still contains deleted name")
		}
		if !slices.Contains(names, keep) {
			t.Fatalf("List dropped a name we kept")
		}
	})

	// The owner-scoping contract (migration 0050, member BYOK): For(owner) is
	// part of the Store interface, not a pg-only feature, so every
	// implementation — the shipped pg default and any future alternate — is
	// held to the same fallback/isolation rules here. Each case is its own
	// top-level func (rather than inline, like the ones above) purely to keep
	// RunConformance's own cognitive-complexity score down; the fixture and
	// assertions are unchanged either way.
	t.Run("owner_get_never_sees_other_owner_falls_back_to_operator", func(t *testing.T) {
		testOwnerGetFallsBackToOperator(t, ctx, newStore, uniq)
	})
	t.Run("owner_put_never_clobbers_operator_row", func(t *testing.T) {
		testOwnerPutNeverClobbersOperatorRow(t, ctx, newStore, uniq)
	})
	t.Run("owner_list_is_own_rows_only", func(t *testing.T) {
		testOwnerListIsOwnRowsOnly(t, ctx, newStore, uniq)
	})
	// for_empty_byte_identical is the negative control on the three cases
	// above: For("") must be indistinguishable from the store's own
	// zero-owner behavior, proving For does not change any EXISTING caller
	// that never invokes it.
	t.Run("for_empty_byte_identical", func(t *testing.T) {
		testForEmptyByteIdentical(t, ctx, newStore, uniq)
	})
	t.Run("delete_owner_row_leaves_operator_row", func(t *testing.T) {
		testDeleteOwnerRowLeavesOperatorRow(t, ctx, newStore, uniq)
	})
}

// newStoreFunc and uniqFunc name the two closures every owner-scoping case
// below shares with RunConformance, so each case's signature stays short.
type newStoreFunc = func(t *testing.T) secretstore.Store
type uniqFunc = func(prefix string) string

func testOwnerGetFallsBackToOperator(t *testing.T, ctx context.Context, newStore newStoreFunc, uniq uniqFunc) {
	op := newStore(t)
	name := uniq("shared-name")
	a, b := uniq("owner-a"), uniq("owner-b")
	t.Cleanup(func() { _ = op.Delete(ctx, name); _ = op.For(a).Delete(ctx, name) })

	if err := op.Put(ctx, name, []byte("operator-value")); err != nil {
		t.Fatalf("Put operator row: %v", err)
	}
	if got, err := op.For(a).Get(ctx, name); err != nil || string(got) != "operator-value" {
		t.Fatalf("For(a).Get before A has a row = (%q, %v), want the operator fallback", got, err)
	}
	if err := op.For(a).Put(ctx, name, []byte("a-value")); err != nil {
		t.Fatalf("For(a).Put: %v", err)
	}

	gotA, err := op.For(a).Get(ctx, name)
	if err != nil || string(gotA) != "a-value" {
		t.Fatalf("For(a).Get after A has a row = (%q, %v), want a-value (A's own row wins)", gotA, err)
	}
	gotB, err := op.For(b).Get(ctx, name)
	if err != nil || string(gotB) != "operator-value" {
		t.Fatalf("For(b).Get = (%q, %v), want operator-value — B holds none, falls back, and never sees A's", gotB, err)
	}
}

func testOwnerPutNeverClobbersOperatorRow(t *testing.T, ctx context.Context, newStore newStoreFunc, uniq uniqFunc) {
	op := newStore(t)
	name := uniq("clobber-check")
	a := uniq("owner-a")
	t.Cleanup(func() { _ = op.Delete(ctx, name); _ = op.For(a).Delete(ctx, name) })

	if err := op.Put(ctx, name, []byte("operator-value")); err != nil {
		t.Fatalf("Put operator row: %v", err)
	}
	if err := op.For(a).Put(ctx, name, []byte("a-value")); err != nil {
		t.Fatalf("For(a).Put: %v", err)
	}
	got, err := op.Get(ctx, name)
	if err != nil || string(got) != "operator-value" {
		t.Fatalf("operator Get after A's Put = (%q, %v), want operator-value untouched", got, err)
	}
}

func testOwnerListIsOwnRowsOnly(t *testing.T, ctx context.Context, newStore newStoreFunc, uniq uniqFunc) {
	op := newStore(t)
	a, b := uniq("owner-a"), uniq("owner-b")
	opName, aName, bName := uniq("op-name"), uniq("a-name"), uniq("b-name")
	t.Cleanup(func() {
		_ = op.Delete(ctx, opName)
		_ = op.For(a).Delete(ctx, aName)
		_ = op.For(b).Delete(ctx, bName)
	})
	_ = op.Put(ctx, opName, []byte("v"))
	_ = op.For(a).Put(ctx, aName, []byte("v"))
	_ = op.For(b).Put(ctx, bName, []byte("v"))

	names, err := op.For(a).List(ctx)
	if err != nil {
		t.Fatalf("For(a).List: %v", err)
	}
	if !slices.Contains(names, aName) {
		t.Fatalf("For(a).List missing A's own name")
	}
	if slices.Contains(names, opName) {
		t.Fatalf("For(a).List contains the operator name %q; List must be own rows ONLY (a caller composes the union itself)", opName)
	}
	if slices.Contains(names, bName) {
		t.Fatalf("For(a).List contains B's name %q", bName)
	}
}

func testForEmptyByteIdentical(t *testing.T, ctx context.Context, newStore newStoreFunc, uniq uniqFunc) {
	op := newStore(t)
	name := uniq("for-empty")
	t.Cleanup(func() { _ = op.Delete(ctx, name) })
	if err := op.For("").Put(ctx, name, []byte("v")); err != nil {
		t.Fatalf(`For("").Put: %v`, err)
	}
	got, err := op.Get(ctx, name)
	if err != nil || string(got) != "v" {
		t.Fatalf(`plain Get after For("").Put = (%q, %v), want "v"`, got, err)
	}
}

func testDeleteOwnerRowLeavesOperatorRow(t *testing.T, ctx context.Context, newStore newStoreFunc, uniq uniqFunc) {
	op := newStore(t)
	name := uniq("delete-check")
	a := uniq("owner-a")
	t.Cleanup(func() { _ = op.Delete(ctx, name) })
	_ = op.Put(ctx, name, []byte("operator-value"))
	_ = op.For(a).Put(ctx, name, []byte("a-value"))

	if err := op.For(a).Delete(ctx, name); err != nil {
		t.Fatalf("For(a).Delete: %v", err)
	}
	got, err := op.Get(ctx, name)
	if err != nil || string(got) != "operator-value" {
		t.Fatalf("operator Get after A's Delete = (%q, %v), want operator-value still present", got, err)
	}
	gotA, err := op.For(a).Get(ctx, name)
	if err != nil || string(gotA) != "operator-value" {
		t.Fatalf("For(a).Get after deleting A's own row = (%q, %v), want the fallback to operator-value", gotA, err)
	}
}
