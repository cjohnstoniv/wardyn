// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Audit-trigger CATALOG inspection: what pg_trigger and pg_proc actually say
// about the triggers on audit_events, as opposed to what the migrations
// intended to install. The three readers here are what ensureAuditTriggers and
// the boot posture report ask before they decide whether the append-only
// hardening is really in force. Split from db.go by seam (the file-size gate);
// no behaviour lives here that db.go's migration doc does not describe.

import (
	"context"
	"fmt"
	"sort"
)

// auditImpostorTriggers returns the SHIPPED-NAMED triggers on audit_events that
// are bound to something other than the function the migrations bind them to,
// keyed by trigger name with the offending function as `<schema>.<name>` so the
// operator can find it. Empty when every shipped trigger is the one Wardyn
// created, and when the table does not exist.
//
// THE SCHEMA IS PART OF THE COMPARISON, not decoration in the message. Matching
// on proname alone would accept a forger's own `audit_events_chain()` created in
// a schema earlier on the search_path — the same shadowing 0058 exists to stop
// on the resolution side, arriving here through the catalog instead. The
// shipped function always lives in the schema the table does (0001 creates it
// unqualified alongside audit_events; 0058 creates it as `<schema>.
// audit_events_chain` from the table's own namespace), so that is the identity
// tested.
//
// WHAT THIS DOES NOT CATCH, stated so nobody reads it as more than it is: the
// shipped function's BODY, replaced in place with CREATE OR REPLACE FUNCTION.
// The name and the schema still match and the catalog looks identical. That is a
// behavioural question, not a catalog one, and it is what the boot canary answers
// — a rolled-back synthetic append asserting the row actually chains.
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

	// One read, decided in Go: the catalog answers "what does each shipped
	// trigger execute, and where does that function live"; the expectation is
	// the map above, so the two cannot be restated differently in SQL and Go.
	var tgnames, funcs, funcSchemas, tableSchemas []string
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(array_agg(t.tgname::text   ORDER BY t.tgname), ARRAY[]::text[]),
		       COALESCE(array_agg(p.proname::text  ORDER BY t.tgname), ARRAY[]::text[]),
		       COALESCE(array_agg(fn.nspname::text ORDER BY t.tgname), ARRAY[]::text[]),
		       COALESCE(array_agg(tn.nspname::text ORDER BY t.tgname), ARRAY[]::text[])
		FROM pg_trigger t
		JOIN pg_proc p       ON p.oid  = t.tgfoid
		JOIN pg_namespace fn ON fn.oid = p.pronamespace
		JOIN pg_class c      ON c.oid  = t.tgrelid
		JOIN pg_namespace tn ON tn.oid = c.relnamespace
		WHERE t.tgrelid = 'audit_events'::regclass
		  AND NOT t.tgisinternal
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

// auditForeignTriggers returns the non-internal triggers on audit_events that
// Wardyn does not ship and that are not DISABLED, split into the two classes
// that matter. Returns nothing when the table does not exist.
//
// tamperCapable is the class that defeats the whole audit design: a ROW-level
// BEFORE INSERT trigger. It is handed NEW and whatever it returns is what
// Postgres stores, so it can rewrite any field, choose prev_hash/row_hash, or
// RETURN NULL to make the event vanish — and the row it leaves behind is
// internally consistent, so store.VerifyAuditChain reports the log clean. This
// was measured, not assumed: with such a trigger installed, an event submitted
// through store.InsertAuditEvent as actor=X outcome=denied was stored as
// actor=Y outcome=success, InsertAuditEvent returned nil, this boot check
// returned nil, and the sweep returned ok=true.
//
// NAME ORDER IS NOT THE TEST, and reasoning that it is would have left the
// easier attack open. AuditDDLProtected's doc comment and docs/OPERATIONS.md
// both describe this bypass as a trigger sorting AFTER audit_events_chain
// (same-event row triggers fire in name order, so it runs last and overwrites
// the hashes). That is one way to do it. A trigger sorting BEFORE the chain
// trigger is strictly easier: it rewrites NEW and the SHIPPED chain trigger
// then hashes the forgery for it — no name trick, no hash call. Both were
// executed; both left the sweep reporting ok=true. So every foreign row-level
// BEFORE INSERT trigger is refused, whatever it is called.
//
// other is every remaining foreign trigger — AFTER, statement-level, or bound
// to another event. None of them can alter the stored row, so they are reported
// rather than refused: a deployment may legitimately hang a replication or
// notify trigger off this table, and bricking that boot would be a worse
// failure than naming it.
//
// TGENABLED IS NOT auditTriggerNames' FILTER, and reusing that one left the
// state that matters most invisible. auditTriggerNames asks whether one of
// WARDYN'S OWN triggers is firing for ORDINARY writes, so it reads 'O' and 'A'
// and correctly treats 'R' (replica-only) as absent. Asking the same question of
// a FOREIGN trigger inverts the answer: 'R' means dormant for ordinary traffic
// and ARMED for exactly the `session_replication_role = replica` session the
// whole audit design names as the bypass window (docs/OPERATIONS.md,
// AuditDDLProtected's doc comment, the sweep's rule 3 in store.auditChainWalk).
// A forging row-level BEFORE INSERT trigger parked at 'R' therefore passed this
// refusal outright — and with the shipped chain trigger hardened to ENABLE
// ALWAYS, the hardening this file goes out of its way to preserve and which
// fires under replica too, the forged row was hash-chained on the way in: actor
// and outcome the forger's, row_hash present and valid, VerifyAuditChain
// reporting the log clean. So the predicate is stated in ITS OWN terms rather
// than borrowed: any state except 'D'.
//
// 'D' STAYS OUT, deliberately and narrowly. A disabled trigger fires for
// nothing at all, so a catalog row parked at 'D' cannot rewrite a row; arming it
// is an ALTER TABLE ... ENABLE, which needs the very TRIGGER privilege
// AuditDDLProtected exists to report on, and a boot that refused over a trigger
// somebody had neutralised the supported way would fail in the wrong direction.
func auditForeignTriggers(ctx context.Context, db migrationExecutor) (tamperCapable, other []string, err error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, nil, fmt.Errorf("db: look up audit_events: %w", err)
	}
	if !exists {
		return nil, nil, nil
	}
	shipped := append([]string{auditChainTrigger}, auditAppendOnlyTriggers...)
	// pg_trigger.tgtype is the bitmask from Postgres's own trigger.h:
	// 1 = FOR EACH ROW, 2 = BEFORE, 4 = INSERT. So (tgtype & 3) = 3 is a
	// row-level BEFORE trigger and (tgtype & 4) <> 0 means it fires on INSERT.
	const rowBeforeInsert = `(tgtype & 3) = 3 AND (tgtype & 4) <> 0`
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(array_agg(tgname::text ORDER BY tgname) FILTER (WHERE `+rowBeforeInsert+`), ARRAY[]::text[]),
		       COALESCE(array_agg(tgname::text ORDER BY tgname) FILTER (WHERE NOT (`+rowBeforeInsert+`)), ARRAY[]::text[])
		FROM pg_trigger
		WHERE tgrelid = 'audit_events'::regclass
		  AND NOT tgisinternal
		  AND tgenabled <> 'D'
		  AND tgname <> ALL($1)`, shipped,
	).Scan(&tamperCapable, &other); err != nil {
		return nil, nil, fmt.Errorf("db: read foreign audit_events triggers: %w", err)
	}
	return tamperCapable, other, nil
}

// auditTriggerNames returns the FIRING row/statement triggers on audit_events,
// or nil when the table does not exist. Disabled is treated as absent on
// purpose: ALTER TABLE ... DISABLE TRIGGER leaves the catalog row in place, so a
// check that only asked whether the trigger EXISTS would pass on a table where
// it never fires.
//
// Firing is tgenabled 'O' (origin, the shipped state) OR 'A' (ALWAYS). 'A' is a
// HARDENING, not a deviation: an ALWAYS trigger fires even under
// session_replication_role = replica, which is exactly the bypass the sweep's
// rule 3 exists to catch after the fact (store.auditChainWalk) — an operator who
// applies it is closing that hole at the source. Reading 'A' as absent would
// have made the next boot log the chain trigger as missing, DROP and re-create
// it as plain 'O' (silently reverting the hardening), and an ALWAYS append-only
// trigger would have made Migrate refuse the boot outright — bricking the
// upgrade of the most careful deployments. 'D' (disabled) and 'R' (replica-only,
// which does NOT fire for ordinary writes) stay absent, correctly.
//
// This is only the READ half of what 'A' means. Reading it as firing is not
// enough on its own: every migration that (re)defines an audit trigger ends in
// CREATE TRIGGER, which always yields 'O', so the migration loop reverted the
// hardening this function is careful not to punish. restoreAlwaysTriggers is the
// WRITE half, and the two must keep agreeing — 'A' is a hardening to be
// preserved, never a deviation to be normalised.
func auditTriggerNames(ctx context.Context, db migrationExecutor) (map[string]bool, error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('audit_events') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("db: look up audit_events: %w", err)
	}
	if !exists {
		return nil, nil
	}
	var names []string
	if err := db.QueryRow(ctx, `
		SELECT COALESCE(array_agg(tgname::text), ARRAY[]::text[])
		FROM pg_trigger
		WHERE tgrelid = 'audit_events'::regclass AND NOT tgisinternal AND tgenabled IN ('O', 'A')`,
	).Scan(&names); err != nil {
		return nil, fmt.Errorf("db: read audit_events triggers: %w", err)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}
