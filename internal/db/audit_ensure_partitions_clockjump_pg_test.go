// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// The clock-jump lockout (0138): audit_append pins recorded_at to the high-water mark, so a mark left past
// every created partition by a database clock that stepped forward and came back made every append fail and
// audit_ensure_partitions create nothing. Guarded by WARDYN_TEST_PG like every *_pg_test.go here.

import (
	"context"
	"strings"
	"testing"
)

func TestPG_AuditEnsurePartitions_FollowsTheHighWaterMarkAfterAClockJump(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t, 2)
	f.convert(t)
	app := f.app(t)

	partitions := func() int {
		return pgScalar[int](t, f.owner, `SELECT jsonb_array_length(manifest) FROM audit_partition_meta`)
	}
	before := partitions()

	// One append recorded under a database clock stepped forward past every partition (the manifest
	// reaches 12 months ahead), then the clock is corrected: the mark stays where it was. The mark is moved
	// as the owner, which is what that one append does.
	pgExec(t, f.owner, `UPDATE audit_partition_meta
		SET hw_recorded_at = date_trunc('month', clock_timestamp()) + interval '15 months'`)

	// The lockout: no partition exists for the row's pinned time.
	if _, err := app.Exec(ctx, `SELECT audit_append(gen_random_uuid(), now(), NULL, 'system', 'clock-jump', 'test.clockjump', '', 'success', '', NULL)`); err == nil ||
		!strings.Contains(err.Error(), "no partition") {
		t.Fatalf("an append with the mark past every partition = %v, want the no-partition failure this test reproduces", err)
	}

	// The boot's own call, as the app role, now creates the months the mark points at.
	if err := ensureAuditPartitions(ctx, app); err != nil {
		t.Fatalf("ensureAuditPartitions: %v", err)
	}
	if after := partitions(); after <= before {
		t.Fatalf("audit_ensure_partitions created nothing for a mark past every partition (%d -> %d partitions)", before, after)
	}
	if !pgScalar[bool](t, f.owner, `SELECT (SELECT max((e->>'hi')::timestamptz) FROM audit_partition_meta, jsonb_array_elements(manifest) e)
		>= date_trunc('month', (SELECT hw_recorded_at FROM audit_partition_meta)) + interval '13 months'`) {
		t.Error("the newest partition does not reach 12 months past the month of the high-water mark")
	}

	// The writes and the boot canary work again, with nothing done by hand.
	if seq, _, hash := appendAudit(t, app, "test.clockjump.recovered"); seq == 0 || hash == "" {
		t.Errorf("append after the repair = seq %d hash %q", seq, hash)
	}
	if err := AuditChainCanary(ctx, app); err != nil {
		t.Errorf("the boot canary still refuses: %v", err)
	}
	// Idempotent, and the grant survived the replacement: a second call as the app role creates nothing.
	if n := pgScalar[int](t, app, `SELECT audit_ensure_partitions(12)`); n != 0 {
		t.Errorf("a second audit_ensure_partitions created %d partitions", n)
	}
}
