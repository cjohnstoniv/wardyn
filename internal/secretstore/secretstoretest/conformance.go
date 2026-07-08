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
}
