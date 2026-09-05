// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the review finding that a migration failing AFTER a trigger-redefining
// one permanently and silently stripped an operator's ENABLE ALWAYS hardening
// off audit_events_chain.
//
// TestMigrateKeepsAnAlwaysTriggerAcrossAnUpgrade covers the SUCCESS path only:
// every migration applies, migrateOn reaches its tail, and the restore runs. The
// failure path is not an exotic one — it is the DOCUMENTED remediation path for
// two migrations in this very release (0059's colliding home_override, 0060's
// api_tokens.role outside the closed set both fail loudly on purpose and tell the
// operator to fix the data and re-run). On that path migrateOn returned before
// the restore: 0056-0058 had already COMMITTED their DROP TRIGGER + CREATE
// TRIGGER, so tgenabled was 'O', nothing was logged, and the hardening could not
// be recovered by the remediated boot either — that boot's capture reads the
// reverted 'O' and 0056-0058 are recorded applied, so there is nothing left to
// restore and nothing left to notice.
//
// The probe migrates into its OWN schema, so nothing here touches the lane's
// audit_events or its api_tokens.
//
// docs/OPERATIONS.md:164-175 is the promise under test: an operator's hardening
// survives an upgrade "exactly as it is", and if it cannot be re-applied the boot
// log says so at ERROR and names the statement to run.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPG_MigrateKeepsAnAlwaysTriggerAcrossAFAILEDMigration(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	// Harden the chain trigger the way docs/OPERATIONS.md tells an operator to.
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events ENABLE ALWAYS TRIGGER `+auditChainTrigger); err != nil {
		t.Skipf("cannot ENABLE ALWAYS as this role (%v); the test needs table ownership", err)
	}
	if got := auditTriggerState(t, pool, auditChainTrigger); got != "A" {
		t.Fatalf("precondition: tgenabled = %q after ENABLE ALWAYS, want 'A'", got)
	}

	// Arm the failure the way a real deployment arms it: a row 0060's CHECK
	// refuses. 0060 is chosen because its own comment nominates "fix the rows
	// and re-run" as the supported response, which is precisely the boot this
	// finding says loses the hardening.
	const failing = "0060_api_tokens_role_check.sql"
	if _, err := pool.Exec(ctx, `ALTER TABLE api_tokens DROP CONSTRAINT IF EXISTS api_tokens_role_check`); err != nil {
		t.Fatalf("drop 0060's constraint in %s: %v", schema, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_tokens (id, principal, role, token_sha256) VALUES ($1, 'drifted@example.com', 'owner', $2)`,
		uuid.New(), uuid.NewString()); err != nil {
		t.Fatalf("seed the drifted api_tokens row in %s: %v", schema, err)
	}

	// Re-open the upgrade: the trigger-defining files (which revert 'A' to 'O'
	// on their way through) AND the file that will fail after them.
	pending := append(chainTriggerMigrations(t), failing)
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE filename = ANY($1)`, pending); err != nil {
		t.Fatalf("un-apply %v: %v", pending, err)
	}

	err := Migrate(ctx, pool)
	if err == nil {
		t.Fatalf("Migrate returned nil; the probe did not reproduce a FAILING migration and so cannot test the failure path")
	}
	if !strings.Contains(err.Error(), failing) {
		t.Fatalf("Migrate failed on %v, want a failure in %s — the probe is testing a different failure than it claims", err, failing)
	}

	if got := auditTriggerState(t, pool, auditChainTrigger); got != "A" {
		t.Fatalf("tgenabled = %q after a FAILED Migrate, want 'A'.\n"+
			"0056-0058 committed their DROP TRIGGER + CREATE TRIGGER before %s failed, so the operator's ENABLE ALWAYS "+
			"hardening is gone — and it is gone for good: the next boot's capture reads this 'O' as the shipped state and "+
			"those files are already recorded applied, so nothing re-applies it and nothing says so. "+
			"docs/OPERATIONS.md promises the hardening survives an upgrade exactly as it is.", got, failing)
	}

	// And the remediated boot: fix the data, run again, still 'A'. Without this
	// the test would accept a restore that only held while the failure stood.
	if _, err := pool.Exec(ctx, `UPDATE api_tokens SET role = 'member' WHERE role = 'owner'`); err != nil {
		t.Fatalf("remediate the drifted row: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after remediating the row: %v", err)
	}
	if got := auditTriggerState(t, pool, auditChainTrigger); got != "A" {
		t.Errorf("tgenabled = %q after the remediated Migrate, want 'A'", got)
	}
}
