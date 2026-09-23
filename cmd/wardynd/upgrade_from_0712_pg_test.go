// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// A database exactly as v0.7.12 left it boots on this version (T-14, #674).
// 0.7.12 shipped the envelope migration as 0065_secret_envelope_v1.sql; this
// branch carries the same statements as 0069_secret_envelope_v1.sql, and
// migrations are tracked by FILENAME, so the upgrade re-runs them over columns
// that already exist (internal/db TestBackportedMigrationsStayIdempotent keeps
// that safe). internal/db/testdata/migrations-0.7.12 is the release's own set,
// frozen, so a later edit to a shared migration on this branch cannot make the
// fixture drift toward what it is meant to check.
//
// MP-4a (#548) and MP-4b (#549) add their boot conversions to this test when
// they land: the PENDING credential_reauth row below is the one MP-4a cancels.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

const release0712Migrations = "../../internal/db/testdata/migrations-0.7.12"

// apply0712Migrations applies the release's files the way its Migrate did: one
// transaction per file, each recorded by filename.
func apply0712Migrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(release0712Migrations, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("0.7.12 migration fixture: %v (%d files)", err, len(files))
	}
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (filename TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("0.7.12 migration %s: %v", filepath.Base(f), err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (filename) VALUES ($1)`, filepath.Base(f)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

type sealedRow struct {
	version            int16
	kekID, wrapped, ct string
}

func sealedRows(t *testing.T, pool *pgxpool.Pool) map[string]sealedRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext FROM secrets`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]sealedRow{}
	for rows.Next() {
		var owner, name string
		var r sealedRow
		var wrapped, ct []byte
		if err := rows.Scan(&owner, &name, &r.version, &r.kekID, &wrapped, &ct); err != nil {
			t.Fatal(err)
		}
		r.wrapped, r.ct = string(wrapped), string(ct)
		out[owner+"/"+name] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func appliedMigrations(t *testing.T, pool *pgxpool.Pool) map[string]time.Time {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT filename, applied_at FROM schema_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var name string
		var at time.Time
		if err := rows.Scan(&name, &at); err != nil {
			t.Fatal(err)
		}
		out[name] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPG_UpgradeFrom_0_7_12(t *testing.T) {
	pool := throwawayDB(t)
	ctx := t.Context()
	apply0712Migrations(t, pool)

	// What 0.7.12 left behind: a pre-envelope signing key its first boot
	// converted, a boot key and a person's credential it wrote as v1. Its
	// secret store is this one (the #594 backport, format pinned by the kek
	// golden vectors), so this package writes them.
	id, _ := age.GenerateX25519Identity()
	signing, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signingPEM, err := marshalECPrivateKeyPEM(signing)
	if err != nil {
		t.Fatal(err)
	}
	seedV0Row(t, pool, id, secretSigningKey, signingPEM)
	store0712, err := buildSecretStore(ctx, pool, id.String(), "")
	if err != nil {
		t.Fatalf("0.7.12 boot: %v", err)
	}
	session, err := loadOrCreateSessionKey(ctx, store0712)
	if err != nil {
		t.Fatal(err)
	}
	const owner, cred, credValue = "alice@example.test", "github-pat", "upgrade-canary-value"
	if err := store0712.For(owner).Put(ctx, cred, []byte(credValue)); err != nil {
		t.Fatal(err)
	}
	runID, reauthID, mappingID := uuid.New(), uuid.New(), uuid.New()
	const siteConfig = `{"public_url": "https://wardyn.example.test"}`
	for _, seed := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO role_mappings (id, value, role, created_by) VALUES ($1, 'wardyn-security', 'security_admin', 'admin@example.test')`, []any{mappingID}},
		{`INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		  VALUES ($1, $2, 'claude-code', '', 'CC2', 'RUNNING', 'spiffe://wardyn/run/upgrade', 'docker')`, []any{runID, owner}},
		{`INSERT INTO approvals (id, run_id, kind, requested_scope) VALUES ($1, $2, 'credential_reauth', '{"credential": "aws-sso"}')`, []any{reauthID, runID}},
		{`INSERT INTO site_config (config) VALUES ($1::jsonb) ON CONFLICT (singleton) DO UPDATE SET config = EXCLUDED.config`, []any{siteConfig}},
	} {
		if _, err := pool.Exec(ctx, seed.sql, seed.args...); err != nil {
			t.Fatalf("seed 0.7.12 row: %v", err)
		}
	}
	sealed0712 := sealedRows(t, pool)
	for key, r := range sealed0712 {
		if r.version != 1 {
			t.Fatalf("fixture: %s is enc_version %d after the 0.7.12 boot, want 1", key, r.version)
		}
	}

	boot := func(label string) {
		t.Helper()
		if err := db.Migrate(ctx, pool); err != nil {
			t.Fatalf("%s: migrate a 0.7.12 database: %v", label, err)
		}
		secrets, err := buildSecretStore(ctx, pool, id.String(), "")
		if err != nil {
			t.Fatalf("%s: secret store over a 0.7.12 database: %v", label, err)
		}
		// Byte-for-byte: every row was already v1, so a boot that re-sealed,
		// re-wrapped or re-minted one did work it had no reason to do.
		if got := sealedRows(t, pool); !maps.Equal(got, sealed0712) {
			t.Fatalf("%s rewrote stored secrets: %d rows before, %d after, or their bytes changed", label, len(sealed0712), len(got))
		}
		gotSigning, err := loadOrCreateSigningKey(ctx, secrets)
		if err != nil {
			t.Fatalf("%s: signing key: %v", label, err)
		}
		gotSession, err := loadOrCreateSessionKey(ctx, secrets)
		if err != nil {
			t.Fatalf("%s: session key: %v", label, err)
		}
		gotCred, err := secrets.For(owner).Get(ctx, cred)
		if err != nil {
			t.Fatalf("%s: %s's %s: %v", label, owner, cred, err)
		}
		if !gotSigning.Equal(signing) || !bytes.Equal(gotSession, session) || string(gotCred) != credValue {
			t.Fatalf("%s read a different value than 0.7.12 stored (signing same: %v, session same: %v, credential same: %v)",
				label, gotSigning.Equal(signing), bytes.Equal(gotSession, session), string(gotCred) == credValue)
		}
	}

	boot("first boot")
	applied := appliedMigrations(t, pool)
	entries, err := os.ReadDir("../../internal/db/migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := applied[e.Name()]; !ok {
			t.Errorf("%s is not recorded after the upgrade", e.Name())
		}
	}
	if _, ok := applied["0065_secret_envelope_v1.sql"]; !ok {
		t.Error("the 0.7.12 backport's own record is gone; 0.7.12 filenames stay recorded")
	}

	var role, runState, autonomy, runLimits, approvalKind, approvalState string
	var configSame, standardType bool
	if err := pool.QueryRow(ctx, `
		SELECT m.role, r.state, r.autonomy_level, r.run_limits::text, a.kind, a.state,
		       (SELECT config = $4::jsonb FROM site_config),
		       EXISTS (SELECT 1 FROM user_types WHERE id = 'standard')
		  FROM role_mappings m, agent_runs r, approvals a
		 WHERE m.id = $1 AND r.id = $2 AND a.id = $3`, mappingID, runID, reauthID, siteConfig,
	).Scan(&role, &runState, &autonomy, &runLimits, &approvalKind, &approvalState, &configSame, &standardType); err != nil {
		t.Fatalf("read the 0.7.12 rows back: %v", err)
	}
	for _, c := range []struct{ what, got, want string }{
		{"role mapping", role, "security_admin"},
		{"run state", runState, "RUNNING"},
		{"run autonomy_level (0065 default)", autonomy, ""},
		{"run run_limits (0072 default)", runLimits, "{}"},
		{"approval kind", approvalKind, "credential_reauth"},
		{"approval state", approvalState, "PENDING"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q after the upgrade, want %q", c.what, c.got, c.want)
		}
	}
	if !configSame || !standardType {
		t.Errorf("site config kept: %v; built-in user type seeded: %v", configSame, standardType)
	}

	boot("second boot")
	again := appliedMigrations(t, pool)
	if !maps.EqualFunc(again, applied, time.Time.Equal) {
		t.Fatalf("the second boot changed schema_migrations: %d rows before, %d after", len(applied), len(again))
	}
}
