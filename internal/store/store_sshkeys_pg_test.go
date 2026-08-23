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

// TestPG_SSHKeys_RoleCheckedAtRoundTripsAndRefreshes covers migration 0046: a
// key added with no RoleCheckedAt (the pre-0046 posture — nil, never
// refreshed) round-trips as nil, not a zero time.Time; a key added WITH one
// (the post-0046 registration posture) round-trips it; and
// RefreshSSHKeyRoles — the OIDC callback's OnLogin hook — re-stamps BOTH role
// and role_checked_at for every key a principal owns, leaving a DIFFERENT
// principal's key untouched.
func TestPG_SSHKeys_RoleCheckedAtRoundTripsAndRefreshes(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	// A key registered with no RoleCheckedAt stamp at all — nil must survive
	// the round trip as nil, never silently becoming a zero time.Time (which
	// would read as "checked at the Unix epoch", i.e. maximally stale but NOT
	// the same "never checked" signal sshAuth's freshness check relies on).
	neverChecked := types.SSHPublicKey{
		Fingerprint: "SHA256:never-checked-" + t.Name(),
		Principal:   "alice2@example.com",
		PublicKey:   "ssh-ed25519 AAAAtest2 alice2@laptop",
		Role:        "admin",
		CreatedAt:   time.Now().UTC(),
	}
	addedNever, err := st.AddSSHKey(ctx, neverChecked)
	if err != nil {
		t.Fatalf("add (no role_checked_at): %v", err)
	}
	if addedNever.RoleCheckedAt != nil {
		t.Errorf("RoleCheckedAt = %v, want nil for a key added with none", addedNever.RoleCheckedAt)
	}
	got, err := st.GetSSHKeyByFingerprint(ctx, neverChecked.Fingerprint)
	if err != nil {
		t.Fatalf("get (no role_checked_at): %v", err)
	}
	if got.RoleCheckedAt != nil {
		t.Errorf("re-fetched RoleCheckedAt = %v, want nil", got.RoleCheckedAt)
	}

	// A key registered WITH a stamp (the 0046 handleAddSSHKey posture): it
	// round-trips, truncated to Postgres's microsecond precision.
	checkedAt := time.Now().UTC().Truncate(time.Microsecond)
	stamped := types.SSHPublicKey{
		Fingerprint:   "SHA256:stamped-" + t.Name(),
		Principal:     "bob2@example.com",
		PublicKey:     "ssh-ed25519 AAAAtest3 bob2@laptop",
		Role:          "member",
		RoleCheckedAt: &checkedAt,
		CreatedAt:     time.Now().UTC(),
	}
	if _, err := st.AddSSHKey(ctx, stamped); err != nil {
		t.Fatalf("add (with role_checked_at): %v", err)
	}
	got, err = st.GetSSHKeyByFingerprint(ctx, stamped.Fingerprint)
	if err != nil {
		t.Fatalf("get (with role_checked_at): %v", err)
	}
	if got.RoleCheckedAt == nil || !got.RoleCheckedAt.Equal(checkedAt) {
		t.Errorf("RoleCheckedAt = %v, want %v", got.RoleCheckedAt, checkedAt)
	}

	// RefreshSSHKeyRoles: the OIDC login hook. bob2 logs back in as admin —
	// their key's role AND role_checked_at both re-stamp.
	refreshedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.RefreshSSHKeyRoles(ctx, stamped.Principal, "admin", refreshedAt); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, err = st.GetSSHKeyByFingerprint(ctx, stamped.Fingerprint)
	if err != nil {
		t.Fatalf("get after refresh: %v", err)
	}
	if got.Role != "admin" {
		t.Errorf("Role after refresh = %q, want admin (login re-stamps the CURRENT role, not just the timestamp)", got.Role)
	}
	if got.RoleCheckedAt == nil || !got.RoleCheckedAt.Equal(refreshedAt) {
		t.Errorf("RoleCheckedAt after refresh = %v, want %v", got.RoleCheckedAt, refreshedAt)
	}

	// alice2's key is a DIFFERENT principal — bob2's login must not touch it.
	untouched, err := st.GetSSHKeyByFingerprint(ctx, neverChecked.Fingerprint)
	if err != nil {
		t.Fatalf("get alice2's key after bob2's refresh: %v", err)
	}
	if untouched.Role != "admin" || untouched.RoleCheckedAt != nil {
		t.Errorf("alice2's key changed by bob2's refresh: role=%q role_checked_at=%v", untouched.Role, untouched.RoleCheckedAt)
	}

	// RefreshSSHKeyRoles for a principal with no registered keys is a
	// silent, successful no-op — logging in has nothing to refresh.
	if err := st.RefreshSSHKeyRoles(ctx, "nobody@example.com", "admin", time.Now().UTC()); err != nil {
		t.Errorf("refresh for a principal with no keys: %v, want nil error", err)
	}
}
