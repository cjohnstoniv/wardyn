// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the SSH gateway key registry (migration 0033).
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
// Run with: WARDYN_TEST_PG=postgres://... go test ./internal/store/...
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_SSHKeys_AddListGetDelete(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	k := types.SSHPublicKey{
		Fingerprint: "SHA256:test-" + t.Name(),
		Principal:   "alice@example.com",
		Name:        "laptop",
		PublicKey:   "ssh-ed25519 AAAAtest alice@laptop",
		Role:        "admin",
		CreatedAt:   time.Now().UTC(),
	}
	added, err := st.AddSSHKey(ctx, k)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if added.Fingerprint != k.Fingerprint || added.Principal != k.Principal {
		t.Errorf("added = %+v, want fingerprint/principal to round-trip", added)
	}

	// GetSSHKeyByFingerprint is the gateway's pre-auth lookup: unscoped by
	// principal, must resolve the row.
	got, err := st.GetSSHKeyByFingerprint(ctx, k.Fingerprint)
	if err != nil {
		t.Fatalf("get by fingerprint: %v", err)
	}
	if got.Principal != k.Principal {
		t.Errorf("get by fingerprint principal = %q, want %q", got.Principal, k.Principal)
	}
	// The 0043 role column is what the gateway's admin override reads — it must
	// survive the real INSERT/SELECT, not only the in-memory fake.
	if got.Role != "admin" {
		t.Errorf("get by fingerprint role = %q, want admin to round-trip", got.Role)
	}

	// A second AddSSHKey with the SAME fingerprint (even a different principal)
	// conflicts — a fingerprint maps to exactly one owner.
	dup := k
	dup.Principal = "mallory@example.com"
	if _, err := st.AddSSHKey(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate fingerprint add: err = %v, want ErrConflict", err)
	}

	// ListSSHKeysByPrincipal returns only alice's row.
	list, err := st.ListSSHKeysByPrincipal(ctx, k.Principal)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Fingerprint != k.Fingerprint {
		t.Errorf("list = %+v, want exactly the one key added", list)
	}
	if empty, err := st.ListSSHKeysByPrincipal(ctx, "mallory@example.com"); err != nil || len(empty) != 0 {
		t.Errorf("mallory's list = %v, %v — want empty, nil", empty, err)
	}

	// DeleteSSHKey scoped to the WRONG principal must not remove the row
	// (ErrNotFound — indistinguishable from a nonexistent fingerprint).
	if err := st.DeleteSSHKey(ctx, k.Fingerprint, "mallory@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete by wrong principal: err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetSSHKeyByFingerprint(ctx, k.Fingerprint); err != nil {
		t.Fatalf("key survived a same-fingerprint wrong-principal delete attempt, but a re-fetch failed: %v", err)
	}

	// DeleteSSHKey scoped to the OWNER removes it.
	if err := st.DeleteSSHKey(ctx, k.Fingerprint, k.Principal); err != nil {
		t.Fatalf("delete by owner: %v", err)
	}
	if _, err := st.GetSSHKeyByFingerprint(ctx, k.Fingerprint); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get after delete: err = %v, want ErrNotFound", err)
	}
}
