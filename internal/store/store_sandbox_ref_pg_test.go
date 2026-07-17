// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the sandbox ref -> substrate rows (the orchestrator's
// RefStore seam, migration 0021). Guarded by WARDYN_TEST_PG.
// Run with: WARDYN_TEST_PG=postgres://... go test ./internal/store/...
package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

func TestPG_SandboxRefCRUD(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	ref := "wardyn-agent-" + uuid.NewString()
	t.Cleanup(func() { _ = s.DeleteRef(context.Background(), ref) })

	// Missing row: found=false, nil error (pre-migration / unknown refs must
	// not error the read path).
	if _, found, err := s.GetRef(ctx, ref); err != nil || found {
		t.Fatalf("GetRef missing row: found=%v err=%v, want false,nil", found, err)
	}

	if err := s.PutRef(ctx, ref, "docker"); err != nil {
		t.Fatalf("PutRef: %v", err)
	}
	if name, found, err := s.GetRef(ctx, ref); err != nil || !found || name != "docker" {
		t.Fatalf("GetRef = %q,%v,%v, want docker,true,nil", name, found, err)
	}

	// Upsert: a second Put on the same ref overwrites the name.
	if err := s.PutRef(ctx, ref, "smolvm"); err != nil {
		t.Fatalf("PutRef upsert: %v", err)
	}
	if name, _, err := s.GetRef(ctx, ref); err != nil || name != "smolvm" {
		t.Fatalf("GetRef after upsert = %q,%v, want smolvm,nil", name, err)
	}

	// Delete, then again: idempotent.
	if err := s.DeleteRef(ctx, ref); err != nil {
		t.Fatalf("DeleteRef: %v", err)
	}
	if _, found, err := s.GetRef(ctx, ref); err != nil || found {
		t.Fatalf("GetRef after delete: found=%v err=%v, want false,nil", found, err)
	}
	if err := s.DeleteRef(ctx, ref); err != nil {
		t.Fatalf("DeleteRef of missing ref must be nil: %v", err)
	}
}
