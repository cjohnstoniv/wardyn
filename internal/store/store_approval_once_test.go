// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// An empty jti is refused before any SQL: written into minted_jti it would
// leave the approval looking unspent, so a `once` approval could be spent again.
// The zero PG has no pool, so reaching the UPDATE would panic.
func TestSpendApprovalOnce_EmptyJTIRefusedBeforeSQL(t *testing.T) {
	spent, err := store.PG{}.SpendApprovalOnce(context.Background(), uuid.New(), "")
	if err == nil || spent {
		t.Fatalf("SpendApprovalOnce(empty jti) = (%v, %v), want (false, error)", spent, err)
	}
}
