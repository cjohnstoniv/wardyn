// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Live tests for audit_checkpoints' posture: append-only for everyone, the table owner included, and written
// only by the roles that may append to the audit log.

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const checkpointInsertSQL = `INSERT INTO audit_checkpoints (seq_lo, seq_hi, kid, statement, signature)
	VALUES ($1, $2, 'k', '{}', decode(repeat('00', 64), 'hex'))`

// TestAuditCheckpoints_AppendOnly: once written, a checkpoint cannot be updated, deleted or truncated, even
// by the table owner; and the table refuses a signature that is not 64 bytes or a range that runs backwards.
func TestAuditCheckpoints_AppendOnly(t *testing.T) {
	ctx := context.Background()
	pool := pgPoolIsolated(t)
	pgExec(t, pool, checkpointInsertSQL, 1, 10)

	for name, sql := range map[string]string{
		"UPDATE":   `UPDATE audit_checkpoints SET kid = 'other'`,
		"DELETE":   `DELETE FROM audit_checkpoints`,
		"TRUNCATE": `TRUNCATE audit_checkpoints`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s as the owner: %v, want the append-only refusal", name, err)
		}
	}
	if got := pgScalar[int](t, pool, `SELECT count(*)::int FROM audit_checkpoints`); got != 1 {
		t.Fatalf("checkpoints after the refused writes = %d, want 1", got)
	}

	for name, sql := range map[string]string{
		"short signature": `INSERT INTO audit_checkpoints (seq_lo, seq_hi, kid, statement, signature) VALUES (20, 30, 'k', '{}', '\x00')`,
		"backwards range": `INSERT INTO audit_checkpoints (seq_lo, seq_hi, kid, statement, signature) VALUES (40, 39, 'k', '{}', decode(repeat('00', 64), 'hex'))`,
		"same seq_lo":     `INSERT INTO audit_checkpoints (seq_lo, seq_hi, kid, statement, signature) VALUES (1, 50, 'k', '{}', decode(repeat('00', 64), 'hex'))`,
		"same seq_hi":     `INSERT INTO audit_checkpoints (seq_lo, seq_hi, kid, statement, signature) VALUES (5, 10, 'k', '{}', decode(repeat('00', 64), 'hex'))`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: the insert was accepted", name)
		}
	}
}

// TestAuditCheckpoints_SplitRoleGrants: on a split-role install the app role (the one holding EXECUTE on
// audit_append, found from the catalog, not by name) may INSERT and SELECT checkpoints and nothing else, even with
// default privileges that grant it UPDATE and DELETE on new tables; a role with only CONNECT may do neither.
func TestAuditCheckpoints_SplitRoleGrants(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t, 3)
	// The documented split-role posture hands the app role DML on every table created later.
	pgExec(t, f.owner, fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s`,
		f.schema, f.appRole))
	f.convert(t)
	app := f.app(t)

	nobody, nobodyPass := auditRole(t, pgPool(t), "wardyn_ck_nobody")
	pgExec(t, f.owner, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, f.schema, nobody))
	other := poolAs(t, f.schema, nobody, nobodyPass)

	for priv, want := range map[string]bool{"INSERT": true, "SELECT": true, "UPDATE": false, "DELETE": false, "TRUNCATE": false} {
		if got := pgScalar[bool](t, f.owner, `SELECT has_table_privilege($1, 'audit_checkpoints', $2)`, f.appRole, priv); got != want {
			t.Errorf("app role %s on audit_checkpoints = %v, want %v", priv, got, want)
		}
		if pgScalar[bool](t, f.owner, `SELECT has_table_privilege($1, 'audit_checkpoints', $2)`, nobody, priv) {
			t.Errorf("a role with nothing but CONNECT holds %s on audit_checkpoints", priv)
		}
	}

	if _, err := app.Exec(ctx, checkpointInsertSQL, 1, 10); err != nil {
		t.Fatalf("the app role cannot insert a checkpoint: %v", err)
	}
	var n int
	if err := app.QueryRow(ctx, `SELECT count(*)::int FROM audit_checkpoints`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the app role reads %d checkpoints, %v; want 1", n, err)
	}
	// ON CONFLICT DO NOTHING is the signer's insert; it needs no UPDATE grant.
	if _, err := app.Exec(ctx, checkpointInsertSQL+` ON CONFLICT DO NOTHING`, 1, 10); err != nil {
		t.Fatalf("the app role's conflicting insert: %v, want nothing done", err)
	}
	for name, sql := range map[string]string{
		"UPDATE":   `UPDATE audit_checkpoints SET kid = 'other'`,
		"DELETE":   `DELETE FROM audit_checkpoints`,
		"TRUNCATE": `TRUNCATE audit_checkpoints`,
	} {
		if _, err := app.Exec(ctx, sql); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s as the app role: %v, want permission denied", name, err)
		}
	}
	if _, err := other.Exec(ctx, checkpointInsertSQL, 20, 30); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("an insert by a role with nothing but CONNECT: %v, want permission denied", err)
	}
}
