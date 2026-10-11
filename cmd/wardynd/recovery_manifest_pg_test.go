// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// recoveryFixture is a migrated throwaway database and a set directory holding a spool and its
// .consumed sidecar, with the manifest already written. Guarded by WARDYN_TEST_PG via emptyDatabase.
func recoveryFixture(t *testing.T) (dsn, set string) {
	t.Helper()
	dsn = emptyDatabase(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	pool.Close()

	set = t.TempDir()
	if err := os.Mkdir(filepath.Join(set, "audit"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(set, "audit", "audit-spool.jsonl"), "{\"id\":1}\n")
	writeFixtureFile(t, filepath.Join(set, "audit", "audit-spool.jsonl.consumed"), "9\n")
	if err := createRecoveryManifest(ctx, manifestSources{DSN: dsn, Set: set, SpoolBase: "audit-spool.jsonl", KEKMode: "local", RecordingStore: "pg"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	return dsn, set
}

func writeFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryManifest_CreateThenVerifyPasses(t *testing.T) {
	dsn, set := recoveryFixture(t)
	if err := verifyRecoveryManifest(context.Background(), dsn, set); err != nil {
		t.Fatalf("verify after create: %v", err)
	}
}

func TestRecoveryManifest_VerifyRefusesAfterOneMoreAuditRow(t *testing.T) {
	dsn, set := recoveryFixture(t)
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT * FROM audit_append(gen_random_uuid(), now(), NULL, 'system', 'manifest-probe', 'manifest.probe', '', 'success', '', NULL)`); err != nil {
		pool.Close()
		t.Fatalf("append audit row: %v", err)
	}
	pool.Close()

	err = verifyRecoveryManifest(ctx, dsn, set)
	if err == nil || !strings.Contains(err.Error(), "audit head") {
		t.Fatalf("verify after one more audit row: err = %v, want a refusal naming %q", err, "audit head")
	}
}

func TestRecoveryManifest_VerifyRefusesAChangedConsumedSidecar(t *testing.T) {
	dsn, set := recoveryFixture(t)
	writeFixtureFile(t, filepath.Join(set, "audit", "audit-spool.jsonl.consumed"), "10\n")
	err := verifyRecoveryManifest(context.Background(), dsn, set)
	if err == nil || !strings.Contains(err.Error(), "audit-spool.jsonl.consumed") {
		t.Fatalf("verify with a changed .consumed: err = %v, want a refusal naming the sidecar", err)
	}
}
