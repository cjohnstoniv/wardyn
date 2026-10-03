// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Live tests for audit_partition_digest (0120): the fold it computes, what it refuses, and who may call
// it. The Go side of the same definition (store.PartitionDigest) is pinned to it from the store package.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// foldDigest is the digest definition written out once more, here, with nothing shared with the
// migration or with store.PartitionDigest: d_0 = sha256(header), d_i = sha256(d_{i-1} || hash_i), hex text.
func foldDigest(header string, hashes []string) string {
	sum := sha256.Sum256([]byte(header))
	d := hex.EncodeToString(sum[:])
	for _, h := range hashes {
		sum = sha256.Sum256([]byte(d + h))
		d = hex.EncodeToString(sum[:])
	}
	return d
}

// closeEveryPartition moves the high-water mark far past every partition's upper bound, as the owner, so
// every partition counts as closed. A test-only shortcut: it is what a long enough run of appends does.
func closeEveryPartition(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	pgExec(t, pool, `UPDATE audit_partition_meta SET hw_recorded_at = '2200-01-01'`)
}

// TestPG_AuditPartitionDigest_FoldsTheClosedPartition: the database's digest of the legacy partition
// (chained rows, then hashless pre-chain rows) equals an independent fold, and the empty partition's is d_0.
func TestPG_AuditPartitionDigest_FoldsTheClosedPartition(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t, 6)
	// Pre-chain rows (NULL row_hash, 0047's legacy rows) fold audit_row_hash(NULL, ...).
	pgExec(t, f.owner, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_chain`)
	pgExec(t, f.owner, `INSERT INTO audit_events (id, actor_type, actor, action, outcome, data)
		SELECT gen_random_uuid(), 'human', 'old@example.com', 'legacy.' || g, 'success', jsonb_build_object('n', g)
		  FROM generate_series(1, 3) g`)
	pgExec(t, f.owner, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_chain`)
	f.convert(t)
	app := f.app(t)
	appendAudit(t, app, "test.after.conversion") // the high-water mark passes the legacy bound: it is closed now

	got := pgScalar[string](t, app, `SELECT audit_partition_digest('audit_events_legacy')`)

	var hashes []string
	rows, err := f.owner.Query(ctx, `SELECT COALESCE(row_hash, audit_row_hash(NULL, id, "time", run_id, actor_type, actor,
		action, target, outcome, source_ip, data)) FROM audit_events_legacy ORDER BY seq`)
	if err != nil {
		t.Fatalf("read legacy rows: %v", err)
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatalf("scan: %v", err)
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	if len(hashes) != 9 {
		t.Fatalf("legacy partition holds %d rows, want the 6 chained and 3 hashless", len(hashes))
	}
	header := pgScalar[string](t, f.owner, `
		SELECT jsonb_build_array('audit_events_legacy', min(seq)::text, max(seq)::text,
		       (extract(epoch FROM min(recorded_at)) * 1000000)::bigint::text,
		       (extract(epoch FROM max(recorded_at)) * 1000000)::bigint::text, count(*)::text)::text
		  FROM audit_events_legacy`)
	if want := foldDigest(header, hashes); got != want {
		t.Errorf("audit_partition_digest(legacy) = %s, an independent fold gives %s", got, want)
	}
	if again := pgScalar[string](t, app, `SELECT audit_partition_digest('audit_events_legacy')`); again != got {
		t.Errorf("a closed partition's digest changed between two calls: %s then %s", got, again)
	}

	// An empty partition digests to d_0 = sha256(header): empty ranges, count 0.
	closeEveryPartition(t, f.owner)
	empty := pgScalar[string](t, f.owner, `SELECT m->>'name' FROM audit_partition_meta, jsonb_array_elements(manifest) m ORDER BY m->>'hi' DESC LIMIT 1`)
	if n := pgScalar[int](t, f.owner, fmt.Sprintf(`SELECT count(*)::int FROM %s`, empty)); n != 0 {
		t.Fatalf("fixture: %s holds %d rows, want an empty partition", empty, n)
	}
	d0 := foldDigest(fmt.Sprintf(`["%s", "", "", "", "", "0"]`, empty), nil)
	if got := pgScalar[string](t, f.owner, `SELECT audit_partition_digest($1)`, empty); got != d0 {
		t.Errorf("digest of the empty partition %s = %s, want d_0 %s", empty, got, d0)
	}
}

// TestPG_AuditPartitionDigest_RefusesWhatIsNotAClosedPartition: an open partition (its digest would
// change after it is taken), a table that is not a partition of the audit log, and a name that tries to
// be SQL.
func TestPG_AuditPartitionDigest_RefusesWhatIsNotAClosedPartition(t *testing.T) {
	f := newUpgradeFixture(t, 2)
	f.convert(t)
	live := pgScalar[string](t, f.owner, `SELECT m->>'name' FROM audit_partition_meta, jsonb_array_elements(manifest) m ORDER BY m->>'lo' LIMIT 1`)
	for name, tc := range map[string]struct{ arg, wantErr string }{
		"open":            {live, "still open"},
		"not a partition": {"agent_runs", "is not a partition of audit_events"},
		"a sql name":      {"audit_events_legacy; DROP TABLE agent_runs", "is not a partition of audit_events"},
		"empty":           {"", "is not a partition of audit_events"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.owner.Exec(context.Background(), `SELECT audit_partition_digest($1)`, tc.arg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("audit_partition_digest(%q) error = %v, want one containing %q", tc.arg, err, tc.wantErr)
			}
		})
	}
	if pgScalar[bool](t, f.owner, `SELECT to_regclass('agent_runs') IS NULL`) {
		t.Error("agent_runs is gone: the SQL name was executed")
	}
}

// TestPG_AuditPartitionDigest_GrantsFollowAuditAppend: the roles that may append may digest, the owner's
// PUBLIC default is revoked, and the function is hardened like audit_append (security definer, a pinned
// path that ends with pg_temp).
func TestPG_AuditPartitionDigest_GrantsFollowAuditAppend(t *testing.T) {
	f := newUpgradeFixture(t, 1)
	f.convert(t)
	if !pgScalar[bool](t, f.owner, `SELECT has_function_privilege($1, 'audit_partition_digest(text)', 'EXECUTE')`, f.appRole) {
		t.Errorf("app role %s (which may EXECUTE audit_append) cannot EXECUTE audit_partition_digest", f.appRole)
	}
	const thisFn = `to_regprocedure('audit_partition_digest(text)')`
	if pgScalar[bool](t, f.owner, `SELECT p.proacl IS NULL OR EXISTS (SELECT 1 FROM aclexplode(p.proacl) a
		WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE') FROM pg_proc p WHERE p.oid = `+thisFn) {
		t.Error("audit_partition_digest is executable by PUBLIC")
	}
	cfg := pgScalar[string](t, f.owner, `SELECT array_to_string(proconfig, ',') FROM pg_proc WHERE oid = `+thisFn)
	if !strings.HasSuffix(cfg, ", pg_temp") && !strings.HasSuffix(cfg, ",pg_temp") {
		t.Errorf("audit_partition_digest search_path = %q, want a pinned path ending in pg_temp", cfg)
	}
	if !pgScalar[bool](t, f.owner, `SELECT prosecdef FROM pg_proc WHERE oid = `+thisFn) {
		t.Error("audit_partition_digest is not SECURITY DEFINER")
	}
	op, err := AuditAppendPostureOf(context.Background(), f.app(t))
	if err != nil {
		t.Fatalf("AuditAppendPostureOf: %v", err)
	}
	if len(op.PublicExecute) != 0 {
		t.Errorf("posture reports PUBLIC EXECUTE on %v", op.PublicExecute)
	}
}
