// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestPG_APITokens_IdentityStampedAt: a mint stamps the token, a sign-in's re-stamp moves the stamp
// forward with the role in one statement, and a login never revives a revoked token.
func TestPG_APITokens_IdentityStampedAt(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	person := "alice-" + uuid.NewString()
	raw, revokedRaw := "wdn_"+uuid.NewString(), "wdn_"+uuid.NewString()
	live := seedToken(t, st, person, raw, []string{"eng"})
	revoked := seedToken(t, st, person, revokedRaw, []string{"eng"})

	if live.IdentityStampedAt == nil || time.Since(*live.IdentityStampedAt) > time.Minute {
		t.Fatalf("minted token stamp = %v, want about now", live.IdentityStampedAt)
	}

	// Age the stamps, then revoke one: only a verified sign-in may move a stamp, and never a revoked row's.
	old := time.Now().UTC().Add(-100 * time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `UPDATE api_tokens SET identity_stamped_at = $1 WHERE principal = $2`, old, person); err != nil {
		t.Fatalf("age stamps: %v", err)
	}
	if _, err := st.RevokeAPIToken(ctx, revoked.ID, "", time.Now().UTC()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if err := st.RefreshAPITokenIdentity(ctx, person, "admin", "standard", []string{"eng"}, false); err != nil {
		t.Fatalf("RefreshAPITokenIdentity: %v", err)
	}

	got, err := st.GetAPITokenByRaw(ctx, raw)
	if err != nil {
		t.Fatalf("live token after sign-in: %v", err)
	}
	if got.Role != "admin" || got.IdentityStampedAt == nil || time.Since(*got.IdentityStampedAt) > time.Minute {
		t.Errorf("after sign-in: role %q stamp %v, want admin and a stamp of about now", got.Role, got.IdentityStampedAt)
	}

	if _, err := st.GetAPITokenByRaw(ctx, revokedRaw); err != store.ErrNotFound {
		t.Errorf("revoked token after sign-in: err = %v, want ErrNotFound (a login must not revive it)", err)
	}
	var stamp time.Time
	if err := pool.QueryRow(ctx, `SELECT identity_stamped_at FROM api_tokens WHERE id = $1`, revoked.ID).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if !stamp.Equal(old) {
		t.Errorf("revoked token stamp = %v, want it left at %v", stamp, old)
	}
}
