// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the reopened half of the finding that a migration run which does not
// reach its tail permanently and silently strips an operator's ENABLE ALWAYS
// hardening off audit_events_chain.
//
// The loop-error arm is covered by TestPG_MigrateKeepsAnAlwaysTriggerAcrossAFAILEDMigration.
// This is the SIBLING EXIT that arm's fix could not reach: the restore was
// deferred, but deferred ONTO THE CALLER'S CONTEXT. wardynd gives connect-and-
// migrate a deadline, and migrateOn logs every file's elapsed time precisely so
// a slow one is visible before that deadline turns it fatal — so a context that
// expires between two migrations is a designed-for exit, not an exotic one. On
// that exit the deferred restore was handed the dead context: both of its
// statements failed before reaching the wire, it could only log that it could
// not tell, and 0056-0058 had already committed their DROP TRIGGER + CREATE
// TRIGGER. The hardening was gone, and gone for good, exactly as on the arm that
// was fixed — the next boot's capture reads the reverted 'O' as the shipped
// state and those files are recorded applied.
//
// The cancellation is driven deterministically rather than by timing: the
// executor below cancels the run at the moment migrateOn asks whether the file
// AFTER the last trigger-defining migration is applied, which is the instant the
// hardening has just been reverted and nothing has yet restored it.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// cancelAtMigration is a migrationExecutor that cancels the run just before
// migrateOn's applied-check for one named file.
type cancelAtMigration struct {
	migrationExecutor
	at     string
	cancel context.CancelFunc
	fired  bool
}

func (c *cancelAtMigration) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if !c.fired && strings.Contains(sql, "FROM schema_migrations WHERE filename") && len(args) == 1 {
		if name, ok := args[0].(string); ok && name == c.at {
			c.fired = true
			c.cancel()
		}
	}
	return c.migrationExecutor.QueryRow(ctx, sql, args...)
}

// migrationAfter returns the migration filename that sorts immediately after
// name, derived from the embedded set the loop itself walks.
func migrationAfter(t *testing.T, name string) string {
	t.Helper()
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for i, n := range names {
		if n == name && i+1 < len(names) {
			return names[i+1]
		}
	}
	t.Fatalf("no migration sorts after %s; the probe cannot pick a cancellation point", name)
	return ""
}

func TestPG_MigrateKeepsAnAlwaysTriggerWhenTheBootContextIsCancelled(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	// Harden the chain trigger the way docs/OPERATIONS.md tells an operator to.
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events ENABLE ALWAYS TRIGGER `+auditChainTrigger); err != nil {
		t.Skipf("cannot ENABLE ALWAYS as this role (%v); the test needs table ownership", err)
	}
	if got := auditTriggerState(t, pool, auditChainTrigger); got != "A" {
		t.Fatalf("precondition: tgenabled = %q after ENABLE ALWAYS, want 'A'", got)
	}

	// Re-open the trigger-defining files, which revert 'A' to 'O' on their way
	// through, and pick the cancellation point immediately after the last of
	// them.
	chain := chainTriggerMigrations(t)
	unapplyMigrations(t, pool, chain)
	cancelAt := migrationAfter(t, chain[len(chain)-1])

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ex := &cancelAtMigration{migrationExecutor: pool, at: cancelAt, cancel: cancel}

	err := migrateOn(runCtx, ex)
	if err == nil {
		t.Fatalf("migrateOn returned nil; the probe did not reproduce a cancelled run and so cannot test that exit")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("migrateOn failed with %v, want a context.Canceled — the probe is testing a different exit than it claims", err)
	}
	if !ex.fired {
		t.Fatalf("the cancellation point %s was never reached in schema %s; the probe cancelled somewhere else", cancelAt, schema)
	}

	if got := auditTriggerState(t, pool, auditChainTrigger); got != "A" {
		t.Fatalf("tgenabled = %q after a CANCELLED Migrate, want 'A'.\n"+
			"%v committed their DROP TRIGGER + CREATE TRIGGER before the boot context expired, and the deferred "+
			"restore ran on that same expired context — so every statement it needs fails before it reaches the "+
			"server and the operator's ENABLE ALWAYS hardening is gone. It is gone for good: the next boot's capture "+
			"reads this 'O' as the shipped state and those files are already recorded applied. "+
			"docs/OPERATIONS.md promises the hardening survives an upgrade exactly as it is.", got, chain)
	}

	// And the retried boot — the operator's actual next move — still finds 'A'.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after the cancelled run: %v", err)
	}
	if got := auditTriggerState(t, pool, auditChainTrigger); got != "A" {
		t.Errorf("tgenabled = %q after the retried Migrate, want 'A'", got)
	}
}
