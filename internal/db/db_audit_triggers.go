// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Audit-trigger CATALOG inspection: what pg_trigger and pg_proc actually say about the triggers on
// audit_events, as opposed to what the migrations intended to install. The three readers here are what
// ensureAuditTriggers and the boot posture report ask before deciding whether the append-only hardening is
// really in force.

import (
	"context"
	"fmt"
	"sort"
)

// auditTreeCTE names every relation a write to audit_events can land on: the table itself, and since
// 0108 every partition under it. Before the conversion (and on a database mid-bootstrap) the tree is
// the one table, so every reader here works on both shapes. Callers have already proved
// audit_events exists.
//
// SECURITY: the guards are per RELATION. A row-level trigger on the parent is cloned onto each
// partition, and it is the clone on the leaf that fires for a row routed there, so "the parent has the
// trigger" proves nothing about a leaf whose clone was disabled or dropped.
const auditTreeCTE = `WITH RECURSIVE audit_tree(oid) AS (
		SELECT 'audit_events'::regclass::oid
		UNION ALL
		SELECT i.inhrelid FROM pg_inherits i JOIN audit_tree t ON i.inhparent = t.oid)`

// auditUserTrigger selects the triggers Wardyn or an operator created, as opposed to the ones the
// server generates for constraints. A clone of a partitioned table's trigger is one of them: it is
// tgisinternal on Postgres 13 and not on 14 and later, and what identifies it on every version is a
// non-zero tgparentid. Aliased t.
const auditUserTrigger = `(NOT t.tgisinternal OR t.tgparentid <> 0)`

// auditImpostorTriggers returns the SHIPPED-NAMED triggers on audit_events bound to something other than
// the function the migrations bind them to, keyed by trigger name with the offending function as
// `<schema>.<name>`. Empty when every shipped trigger is the one Wardyn created, and when the table
// doesn't exist.
//
// SECURITY: schema is part of the comparison — matching on proname alone would accept a forger's own
// `audit_events_chain()` created earlier on the search_path. The shipped function always lives in the
// table's own schema.
//
// Not caught: the shipped function's BODY replaced in place via CREATE OR REPLACE FUNCTION (same name and
// schema, identical catalog) — a behavioural question the boot canary answers instead.
func auditImpostorTriggers(ctx context.Context, db migrationExecutor) (map[string]string, error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("db: look up audit_events: %w", err)
	}
	if !exists {
		return nil, nil
	}
	want := auditShippedTriggerFuncs()
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)

	var tgnames, funcs, funcSchemas, tableSchemas []string // one read, decided in Go, against the expectation map above
	if err := db.QueryRow(ctx, auditTreeCTE+`
		SELECT COALESCE(array_agg(t.tgname::text   ORDER BY t.tgname), ARRAY[]::text[]),
		       COALESCE(array_agg(p.proname::text  ORDER BY t.tgname), ARRAY[]::text[]),
		       COALESCE(array_agg(fn.nspname::text ORDER BY t.tgname), ARRAY[]::text[]),
		       COALESCE(array_agg(tn.nspname::text ORDER BY t.tgname), ARRAY[]::text[])
		FROM pg_trigger t
		JOIN audit_tree      ON audit_tree.oid = t.tgrelid
		JOIN pg_proc p       ON p.oid  = t.tgfoid
		JOIN pg_namespace fn ON fn.oid = p.pronamespace
		JOIN pg_class c      ON c.oid  = t.tgrelid
		JOIN pg_namespace tn ON tn.oid = c.relnamespace
		WHERE `+auditUserTrigger+`
		  AND t.tgname = ANY($1)`, names,
	).Scan(&tgnames, &funcs, &funcSchemas, &tableSchemas); err != nil {
		return nil, fmt.Errorf("db: read audit_events trigger functions: %w", err)
	}
	out := make(map[string]string)
	for i, n := range tgnames {
		if i >= len(funcs) || i >= len(funcSchemas) || i >= len(tableSchemas) {
			break
		}
		if funcs[i] == want[n] && funcSchemas[i] == tableSchemas[i] {
			continue
		}
		out[n] = funcSchemas[i] + "." + funcs[i]
	}
	return out, nil
}

// auditForeignTriggers returns the non-internal, non-DISABLED triggers on audit_events that Wardyn doesn't
// ship, split into the two classes that matter. Returns nothing when the table doesn't exist.
//
// SECURITY: tamperCapable is the class that defeats the whole audit design — a ROW-level BEFORE INSERT
// trigger. It's handed NEW and whatever it returns is what Postgres stores, so it can rewrite any field or
// vanish the event, leaving a row that's internally consistent and passes VerifyAuditChain. Holds whether
// the trigger sorts before or after audit_events_chain (name order is not a defense), so every foreign
// row-level BEFORE INSERT trigger is refused regardless of name.
//
// other is every remaining foreign trigger — AFTER, statement-level, or bound to another event — none of
// which can alter the stored row, so it's reported rather than refused (a deployment may legitimately hang
// a replication or notify trigger here).
//
// SECURITY: the filter is any state except 'D', deliberately not reusing auditTriggerNames' 'O'/'A' filter
// — for a FOREIGN trigger, 'R' (replica-only) is ARMED for exactly the `session_replication_role = replica`
// bypass window the audit design names as the threat (a forging trigger parked at 'R' would otherwise slip
// past this refusal, and still get hash-chained by the shipped ENABLE ALWAYS chain trigger). 'D' alone
// stays excluded: a disabled trigger fires for nothing, and re-arming it needs the TRIGGER privilege
// AuditDDLProtected already reports on.
func auditForeignTriggers(ctx context.Context, db migrationExecutor) (tamperCapable, other []string, err error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, nil, fmt.Errorf("db: look up audit_events: %w", err)
	}
	if !exists {
		return nil, nil, nil
	}
	shipped := append([]string{auditChainTrigger}, auditAppendOnlyTriggers...)
	// pg_trigger.tgtype bitmask (Postgres trigger.h): 1=FOR EACH ROW, 2=BEFORE, 4=INSERT. So (tgtype&3)=3
	// is a row-level BEFORE trigger and (tgtype&4)<>0 means it fires on INSERT.
	const rowBeforeInsert = `(tgtype & 3) = 3 AND (tgtype & 4) <> 0`
	if err := db.QueryRow(ctx, auditTreeCTE+`
		SELECT COALESCE(array_agg(DISTINCT tgname::text ORDER BY tgname::text) FILTER (WHERE `+rowBeforeInsert+`), ARRAY[]::text[]),
		       COALESCE(array_agg(DISTINCT tgname::text ORDER BY tgname::text) FILTER (WHERE NOT (`+rowBeforeInsert+`)), ARRAY[]::text[])
		FROM pg_trigger t
		JOIN audit_tree ON audit_tree.oid = t.tgrelid
		WHERE `+auditUserTrigger+`
		  AND tgenabled <> 'D'
		  AND tgname <> ALL($1)`, shipped,
	).Scan(&tamperCapable, &other); err != nil {
		return nil, nil, fmt.Errorf("db: read foreign audit_events triggers: %w", err)
	}
	return tamperCapable, other, nil
}

// auditTriggerNames returns the FIRING row/statement triggers on audit_events (and on every partition
// under it, see auditTreeCTE), or nil when the table
// doesn't exist. Disabled is treated as absent on purpose: DISABLE TRIGGER leaves the catalog row in
// place, so a mere existence check would pass on a table where it never fires.
//
// SECURITY: firing is tgenabled 'O' (shipped state) OR 'A' (ALWAYS, a HARDENING that also fires under
// session_replication_role = replica — the bypass the sweep's rule 3 exists to catch). Reading 'A' as
// absent would make the next boot silently revert it to plain 'O' (or refuse the boot once append-only is
// ALWAYS). 'D' and 'R' (replica-only) stay absent, correctly.
//
// READ half only: restoreAlwaysTriggers is the WRITE half, and the two must keep agreeing that 'A' is a
// hardening to preserve, never a deviation to normalize (every migration's CREATE TRIGGER yields plain 'O').
func auditTriggerNames(ctx context.Context, db migrationExecutor) (map[string]bool, error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("db: look up audit_events: %w", err)
	}
	if !exists {
		return nil, nil
	}
	// A name counts as present only when EVERY relation in the tree fires it: one partition whose
	// clone was disabled is an unguarded write path, whatever the parent says.
	var names []string
	if err := db.QueryRow(ctx, auditTreeCTE+`
		SELECT COALESCE(array_agg(x.tgname ORDER BY x.tgname), ARRAY[]::text[])
		FROM (SELECT t.tgname::text AS tgname, count(DISTINCT t.tgrelid) AS n
		        FROM pg_trigger t JOIN audit_tree ON audit_tree.oid = t.tgrelid
		       WHERE `+auditUserTrigger+` AND t.tgenabled IN ('O', 'A')
		       GROUP BY t.tgname) x
		WHERE x.n = (SELECT count(*) FROM audit_tree)`,
	).Scan(&names); err != nil {
		return nil, fmt.Errorf("db: read audit_events triggers: %w", err)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}
