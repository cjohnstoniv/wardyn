// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPG_TokenIdentityStampBackfillsCreatedAt applies 0109 over tokens that exist already: each
// gets its created_at as its stamp, the one time known, so turning WARDYN_ROLE_STAMP_TTL on asks
// a holder to sign in once rather than leaving an unstamped row.
func TestPG_TokenIdentityStampBackfillsCreatedAt(t *testing.T) {
	pool, schema := partialSchemaPool(t, "0109")
	ctx := context.Background()

	id := uuid.New()
	created := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `INSERT INTO api_tokens (id, principal, token_sha256, role, created_at) VALUES ($1, 'p', $2, 'user', $3)`,
		id, uuid.NewString(), created); err != nil {
		t.Fatalf("seed token in %s: %v", schema, err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying 0109+ over existing tokens: %v", err)
	}

	var stamped *time.Time
	if err := pool.QueryRow(ctx, `SELECT identity_stamped_at FROM api_tokens WHERE id = $1`, id).Scan(&stamped); err != nil {
		t.Fatalf("read migrated token: %v", err)
	}
	if stamped == nil || !stamped.Equal(created) {
		t.Errorf("identity_stamped_at = %v, want created_at %v", stamped, created)
	}
}
