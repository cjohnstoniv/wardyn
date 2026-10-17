// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

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

// The same audit seq with another row_hash is not the manifest's pair either: the head is compared whole.
func TestRecoveryManifest_VerifyRefusesTheSameSeqWithAnotherRowHash(t *testing.T) {
	dsn, set := recoveryFixture(t)
	path := filepath.Join(set, recoveryManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`"audit_head_hash":\s*""`)
	if !re.Match(raw) {
		t.Fatalf("the fixture's manifest does not carry an empty audit head hash: %s", raw)
	}
	writeFixtureFile(t, path, string(re.ReplaceAll(raw, []byte(`"audit_head_hash": "other"`))))
	err = verifyRecoveryManifest(context.Background(), dsn, set)
	if err == nil || !strings.Contains(err.Error(), "audit head") {
		t.Fatalf("verify with the head's row_hash changed: err = %v, want a refusal naming %q", err, "audit head")
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

// An empty WARDYN_AUDIT_SPOOL is a supported setting: the manifest records no spool, and verify pairs none.
func TestRecoveryManifest_DisabledSpoolCreatesAndVerifies(t *testing.T) {
	dsn := emptyDatabase(t)
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
	set := t.TempDir()
	in := manifestSources{DSN: dsn, Set: set, SpoolBase: filepath.Base(""), KEKMode: "local", RecordingStore: "pg"}
	if err := createRecoveryManifest(ctx, in); err != nil {
		t.Fatalf("create with the spool disabled: %v", err)
	}
	m, err := readRecoveryManifest(set, "verify the set")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if m.AuditSpool != (recoverySpool{}) {
		t.Errorf("audit_spool = %+v, want none recorded", m.AuditSpool)
	}
	if err := verifyRecoveryManifest(ctx, dsn, set); err != nil {
		t.Fatalf("verify with the spool disabled: %v", err)
	}
}

// A manifest's spool basename is a plain file name or empty; anything else is refused before it is used.
func TestRecoveryManifest_ReadRefusesASpoolPath(t *testing.T) {
	for _, base := range []string{".", "../audit-spool.jsonl", "audit/audit-spool.jsonl"} {
		set := t.TempDir()
		body := `{"manifest":` + strconv.Itoa(recoveryManifestVersion) + `,"audit_spool":{"basename":` + strconv.Quote(base) + `}}`
		writeFixtureFile(t, filepath.Join(set, recoveryManifestFile), body)
		if _, err := readRecoveryManifest(set, "verify the set"); err == nil || !strings.Contains(err.Error(), "plain file name") {
			t.Errorf("basename %q: err = %v, want a refusal", base, err)
		}
	}
}

// At pool_max_conns=1 the lock connection leaves stageFiles no second connection, so create used to hang.
// The deadline turns a regression back into a failure instead of a stuck test.
func TestRecoveryManifest_CreateRefusesAOneConnectionPool(t *testing.T) {
	dsn := emptyDatabase(t)
	pool, err := db.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(context.Background(), pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	pool.Close()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("pool_max_conns", "1")
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = createRecoveryManifest(ctx, manifestSources{DSN: u.String(), Set: t.TempDir(), SpoolBase: "", KEKMode: "local", RecordingStore: "pg"})
	if err == nil || !strings.Contains(err.Error(), "pool_max_conns") {
		t.Fatalf("create at pool_max_conns=1: err = %v, want a refusal naming pool_max_conns", err)
	}
}
