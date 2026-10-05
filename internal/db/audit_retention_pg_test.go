// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Live tests for the privilege and hardening side of 0123 (audit_retention_drop, audit_retention_set_policy
// and their helpers): who may call them, what the app role may write, and how they are pinned. The
// behavioural tests (refusals, anchors, the cooldown, verify) are internal/store's, which has the verifier.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const (
	retentionDropFn      = "audit_retention_drop(text, text, text)"
	retentionSetPolicyFn = "audit_retention_set_policy(integer)"
)

// TestPG_AuditRetention_PrivilegeModel: a role that can only CONNECT cannot call the drop or the policy
// function; the app role can call them (it holds EXECUTE on audit_append, which the grant follows) but can
// write neither the policy nor the anchors directly, and nobody holds PUBLIC EXECUTE.
func TestPG_AuditRetention_PrivilegeModel(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t, 3)
	f.convert(t)
	app := f.app(t)

	nobody, nobodyPass := auditRole(t, pgPool(t), "wardyn_ar_nobody")
	pgExec(t, f.owner, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, f.schema, nobody))
	other := poolAs(t, f.schema, nobody, nobodyPass)

	for name, sql := range map[string]string{
		"drop":       `SELECT audit_retention_drop('audit_events_legacy', 'x', 'someone')`,
		"set_policy": `SELECT audit_retention_set_policy(30)`,
		"partitions": `SELECT * FROM audit_retention_partitions(NULL, false)`,
		"window":     `SELECT audit_retention_window()`,
	} {
		_, err := other.Exec(ctx, sql)
		if err == nil || !strings.Contains(err.Error(), "permission denied for function") {
			t.Errorf("%s as a role with nothing but CONNECT: %v, want permission denied for function", name, err)
		}
	}

	for _, fn := range []string{retentionDropFn, retentionSetPolicyFn, "audit_retention_partitions(text, boolean)", "audit_retention_window()"} {
		if !pgScalar[bool](t, f.owner, `SELECT has_function_privilege($1, to_regprocedure($2), 'EXECUTE')`, f.appRole, fn) {
			t.Errorf("the app role cannot EXECUTE %s", fn)
		}
		if pgScalar[bool](t, f.owner, `SELECT has_function_privilege($1, to_regprocedure($2), 'EXECUTE')`, nobody, fn) {
			t.Errorf("a role with nothing but CONNECT can EXECUTE %s", fn)
		}
	}

	// The app role writes neither the policy nor the anchors, however it tries.
	for name, sql := range map[string]string{
		"UPDATE audit_partition_meta":      `UPDATE audit_partition_meta SET retention_days = 1`,
		"UPDATE pending":                   `UPDATE audit_partition_meta SET pending_days = NULL, pending_effective_at = NULL`,
		"INSERT audit_chain_anchors":       `INSERT INTO audit_chain_anchors (kind, partition_name) VALUES ('drop', 'audit_events_legacy')`,
		"DELETE FROM audit_chain_anchors":  `DELETE FROM audit_chain_anchors`,
		"UPDATE audit_chain_anchors":       `UPDATE audit_chain_anchors SET tail_row_hash = NULL`,
		"DETACH PARTITION":                 `ALTER TABLE audit_events DETACH PARTITION audit_events_legacy`,
		"DROP the legacy partition":        `DROP TABLE audit_events_legacy`,
		"DELETE FROM audit_events_legacy ": `DELETE FROM audit_events_legacy`,
	} {
		if _, err := app.Exec(ctx, sql); err == nil || !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "must be owner") {
			t.Errorf("%s as the app role: %v, want permission denied", name, err)
		}
	}

	// Through the function the app role CAN set the policy, and the cooldown is computed in the database.
	var outcome string
	if err := app.QueryRow(ctx, `SELECT outcome FROM audit_retention_set_policy(90)`).Scan(&outcome); err != nil || outcome != "pending" {
		t.Errorf("app role set_policy(90) = %q, %v, want pending (0 to finite is a decrease)", outcome, err)
	}
	if got := pgScalar[int](t, app, `SELECT audit_retention_window()`); got != 0 {
		t.Errorf("window right after a decrease = %d, want 0 (forever) until the cooldown ends", got)
	}
	// The read helper runs as the app role, which can read every partition it counts, and with retention
	// forever (the default) the drop function refuses the oldest, closed legacy partition for that reason.
	if n := pgScalar[int](t, app, `SELECT count(*)::int FROM audit_retention_partitions(NULL, true)`); n < 2 {
		t.Errorf("the app role sees %d partition(s), want the legacy one and the live ones", n)
	}
	if got := pgScalar[string](t, app, `SELECT part_refusal FROM audit_retention_partitions('audit_events_legacy', false)`); got != "audit_retention_inside_window" {
		t.Errorf("the legacy partition's refusal under retention forever = %q, want audit_retention_inside_window", got)
	}
	appendAudit(t, app, "test.close.legacy") // the high-water mark moves past the legacy bound: it is closed
	if _, err := app.Exec(ctx, `SELECT audit_retention_drop('audit_events_legacy', 'x', 'someone')`); err == nil || !strings.Contains(err.Error(), "audit_retention_inside_window") {
		t.Errorf("drop of the legacy partition under retention forever: %v, want it refused as inside the window", err)
	}
	// A refused drop of an unknown name is refused for that, not for permission.
	if _, err := app.Exec(ctx, `SELECT audit_retention_drop('nope', 'x', 'someone')`); err == nil || !strings.Contains(err.Error(), "is not a partition of audit_events") {
		t.Errorf("drop of an unknown partition as the app role: %v", err)
	}
}

// TestPG_AuditRetention_FunctionsArePinned: SECURITY DEFINER where the writes need it, every search_path
// ends with pg_temp and names the real schema, the lock timeout is bounded, nothing is executable by PUBLIC,
// and the boot posture reports one that becomes so.
func TestPG_AuditRetention_FunctionsArePinned(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t, 3)
	f.convert(t)

	type want struct {
		fn      string
		definer bool
		lockTO  bool
	}
	for _, w := range []want{
		{retentionDropFn, true, true},
		{retentionSetPolicyFn, true, true},
		{"audit_retention_partitions(text, boolean)", false, false},
		{"audit_retention_window()", false, false},
		{"audit_retention_refuse(text, text)", false, false},
	} {
		var definer bool
		var cfg []string
		if err := f.owner.QueryRow(ctx, `SELECT prosecdef, COALESCE(proconfig, ARRAY[]::text[]) FROM pg_proc WHERE oid = to_regprocedure($1)`, w.fn).Scan(&definer, &cfg); err != nil {
			t.Fatalf("%s: %v", w.fn, err)
		}
		if definer != w.definer {
			t.Errorf("%s: SECURITY DEFINER = %v, want %v", w.fn, definer, w.definer)
		}
		var sp string
		for _, c := range cfg {
			if strings.HasPrefix(c, "search_path=") {
				sp = c
			}
		}
		if !strings.HasPrefix(sp, "search_path=pg_catalog, ") || !strings.HasSuffix(sp, ", pg_temp") || !strings.Contains(sp, f.schema) {
			t.Errorf("%s: pinned %q, want pg_catalog, %s, pg_temp", w.fn, sp, f.schema)
		}
		if has := slices.Contains(cfg, "lock_timeout=5s"); has != w.lockTO {
			t.Errorf("%s: lock_timeout=5s = %v, want %v (config %v)", w.fn, has, w.lockTO, cfg)
		}
		if pgScalar[bool](t, f.owner, `SELECT COALESCE((SELECT bool_or(a.grantee = 0) FROM pg_proc p, aclexplode(p.proacl) a WHERE p.oid = to_regprocedure($1)), false)`, w.fn) {
			t.Errorf("%s: PUBLIC holds EXECUTE", w.fn)
		}
	}

	pgExec(t, f.owner, `GRANT EXECUTE ON FUNCTION `+retentionDropFn+` TO PUBLIC`)
	p, err := AuditAppendPostureOf(ctx, f.owner)
	if err != nil {
		t.Fatalf("AuditAppendPostureOf: %v", err)
	}
	if !slices.Equal(p.PublicExecute, []string{"audit_retention_drop"}) {
		t.Errorf("PublicExecute = %v, want [audit_retention_drop]", p.PublicExecute)
	}
}
