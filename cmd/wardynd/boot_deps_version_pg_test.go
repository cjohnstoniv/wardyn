// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/version"
)

// TestConnectAndMigrate_RecordsTheRelease: a boot records the release it runs in deployment_versions
// (migration 0192), which setup status reads to name the defaults an upgrade changed.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestConnectAndMigrate_RecordsTheRelease(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed boot record test")
	}
	pool, err := connectAndMigrate(t.Context(), dsn, "", 30*time.Second, 5*time.Minute, false)
	if err != nil {
		t.Fatalf("connectAndMigrate: %v", err)
	}
	t.Cleanup(pool.Close)
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM deployment_versions WHERE version = $1`, version.Version).Scan(&n); err != nil || n != 1 {
		t.Fatalf("deployment_versions rows for %s = %d (%v), want 1", version.Version, n, err)
	}
}
