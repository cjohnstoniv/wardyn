// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// chainRewriteMigrations are the three files that REWRITE audit_events_chain()
// after 0047 first created it: 0056 moves seq allocation inside the trigger
// under pg_advisory_xact_lock (discarding the identity default's value), 0057
// makes the function SECURITY DEFINER with a pinned search_path, 0058
// re-creates it schema-qualified.
var chainRewriteMigrations = []string{
	"0056_audit_chain_serialize.sql",
	"0057_audit_chain_security_definer.sql",
	"0058_audit_chain_schema_qualified.sql",
}

// appendAuditRow appends one row through whichever audit_events_chain() trigger
// is installed and returns (seq, prev_hash, row_hash) as the trigger computed
// them. Nothing here supplies a hash: choosing one is the thing
// docs/OPERATIONS.md promises no writer can do.
func appendAuditRow(t *testing.T, pool *pgxpool.Pool, action string) (int64, string, string) {
	t.Helper()
	var seq int64
	var prev, row string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		VALUES (gen_random_uuid(), 'system', 'upgrade-probe', $1, 'success')
		RETURNING seq, COALESCE(prev_hash,''), COALESCE(row_hash,'')`, action).Scan(&seq, &prev, &row)
	if err != nil {
		t.Fatalf("append %s: %v", action, err)
	}
	return seq, prev, row
}

// TestPG_AuditChainSurvivesTheTriggerRewriteOnAPopulatedDatabase is F023's
// missing coverage. Every other chain test — internal/store/auditchain_pg_test.go,
// auditchain_f11_probe_pg_test.go, auditchain_locktimeout_pg_test.go and
// internal/db/auditchain_searchpath_pg_test.go — runs against a FULLY migrated
// schema, and the closest one (TestPG_ReplayingTheTriggerMigrationsIsIdempotent)
// replays the trigger set into an EMPTY schema. Nothing exercised the boundary
// every UPGRADED deployment actually crosses: rows already chained by 0047's
// trigger, then 0056/0057/0058 rewriting the function underneath them, then
// more rows appended — with VerifyAuditChain walking old and new as ONE chain.
// A future chain migration that broke continuity for upgraded deployments only
// would have reddened nothing.
func TestPG_AuditChainSurvivesTheTriggerRewriteOnAPopulatedDatabase(t *testing.T) {
	pool := databaseBefore(t, chainRewriteMigrations[0]) // 0047's trigger, not yet rewritten
	ctx := context.Background()

	// Rows written by the ORIGINAL (0047) trigger.
	var beforeHead string
	for _, action := range []string{"test.pre.1", "test.pre.2", "test.pre.3"} {
		_, prev, row := appendAuditRow(t, pool, action)
		if row == "" {
			t.Fatalf("%s: 0047's trigger did not chain the row; the fixture is broken", action)
		}
		if beforeHead != "" && prev != beforeHead {
			t.Fatalf("%s: prev_hash = %q, want the preceding row's %q — the PRE-upgrade chain is already broken", action, prev, beforeHead)
		}
		beforeHead = row
	}

	// The upgrade itself.
	for _, name := range chainRewriteMigrations {
		execMigrationFile(t, pool, name)
	}

	// The first row written by the REWRITTEN trigger must link to the last row
	// written by the original one — the boundary this test exists for.
	_, firstPostPrev, _ := appendAuditRow(t, pool, "test.post.1")
	if firstPostPrev != beforeHead {
		t.Errorf("the first post-upgrade row's prev_hash = %q, want the pre-upgrade head %q — "+
			"the trigger rewrite restarted the chain instead of continuing it", firstPostPrev, beforeHead)
	}
	for _, action := range []string{"test.post.2", "test.post.3"} {
		appendAuditRow(t, pool, action)
	}

	// And the verifier — which walks pre- and post-upgrade rows as one chain —
	// must agree.
	st, err := (store.PG{Pool: pool}).VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("VerifyAuditChain: %v", err)
	}
	if !st.OK {
		t.Fatalf("chain verify across the upgrade: OK=false reason=%q broken_seq=%d", st.Reason, st.BrokenSeq)
	}
	if st.Checked != 6 {
		t.Errorf("checked = %d, want the 6 rows written either side of the upgrade", st.Checked)
	}
	if st.Legacy != 0 {
		t.Errorf("legacy = %d, want 0 — every row here was written through a chaining trigger", st.Legacy)
	}
	if st.HeadHash == "" {
		t.Error("head_hash is empty; the verifier reported no head to compare against a SIEM's record")
	}
}
