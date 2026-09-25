// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
)

// TestPGRevocations_Conformance runs the run-identity kill switch's production
// SQL: the run:<uuid> marker cascade had only ever been exercised against the
// in-memory store, so a Postgres IsRevoked that stopped matching the marker
// would have left every token of a killed run verifying.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPGRevocations_Conformance(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed revocation conformance")
	}
	pool, err := connectAndMigrate(t.Context(), dsn, "", 30*time.Second, 60*time.Second)
	if err != nil {
		t.Fatalf("connectAndMigrate: %v", err)
	}
	t.Cleanup(pool.Close)
	identitytest.RunRevocationConformance(t, func(*testing.T) identity.RevocationStore {
		return &pgRevocations{pool: pool}
	})
}
