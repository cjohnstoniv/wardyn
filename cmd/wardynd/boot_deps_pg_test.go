// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// TestConnectAndMigrate_SeparateBudgets pins that connect and migrate get
// independent timeout budgets, not one shared deadline. With a single ctx
// bounding both db.Connect and db.Migrate, a slow migration (e.g. an index
// build on the unbounded audit table) has no knob separate from the fixed 30s
// connect budget and crash-loops the upgrade. A migrateTimeout of 0 forces
// db.Migrate to fail on its own already-expired deadline while connectTimeout
// stays generous, proving the two are independently controllable.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
// Run: WARDYN_TEST_PG="postgres://wardyn:wardyn@localhost:55434/wardyn?sslmode=disable" \
//        go test ./cmd/wardynd/... -run TestConnectAndMigrate_SeparateBudgets

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestConnectAndMigrate_SeparateBudgets(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed boot-budget test")
	}

	// connectTimeout generous (30s, the production default), migrateTimeout
	// already-expired (0) — if the two shared one deadline, connect itself
	// would fail too; they don't, so it must fail
	// specifically inside Migrate.
	_, err := connectAndMigrate(t.Context(), dsn, "", 30*time.Second, 0)
	if err == nil {
		t.Fatal("connectAndMigrate with migrateTimeout=0: want an error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "migrate:") {
		t.Fatalf("error = %q, want a \"migrate:\"-prefixed failure (proves Connect succeeded under its own 30s budget and only Migrate hit its independent, already-expired one)", err.Error())
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error = %q, want it to name the expired migrateTimeout deadline", err.Error())
	}
}
