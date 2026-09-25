// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestPG_TokenUserTypeBackfillsStandard applies 0076 over tokens minted before
// user types existed: each carries the built-in type afterwards, and an empty
// stamp is refused from then on.
func TestPG_TokenUserTypeBackfillsStandard(t *testing.T) {
	pool, schema := partialSchemaPool(t, "0076")
	ctx := context.Background()

	userTok, adminTok := uuid.New(), uuid.New()
	for _, tok := range []struct {
		id   uuid.UUID
		role string
	}{{userTok, "user"}, {adminTok, "admin"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO api_tokens (id, principal, token_sha256, role) VALUES ($1, $2, $3, $4)`,
			tok.id, tok.role+"@example.com", uuid.NewString(), tok.role); err != nil {
			t.Fatalf("seed %s token in %s: %v", tok.role, schema, err)
		}
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate applying 0076+ over 0.7 tokens: %v", err)
	}

	for _, id := range []uuid.UUID{userTok, adminTok} {
		var userType string
		if err := pool.QueryRow(ctx, `SELECT user_type FROM api_tokens WHERE id = $1`, id).Scan(&userType); err != nil {
			t.Fatalf("read migrated token: %v", err)
		}
		if userType != "standard" {
			t.Errorf("token %s user_type = %q, want standard", id, userType)
		}
	}

	if _, err := pool.Exec(ctx, `INSERT INTO api_tokens (id, principal, token_sha256, user_type) VALUES (gen_random_uuid(), 'x', $1, '')`,
		uuid.NewString()); err == nil {
		t.Error("api_tokens accepted an empty user_type")
	}
}
