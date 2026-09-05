// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the reopened review finding that the boot audit-trigger re-check
// never reports an UNEXPECTED trigger on audit_events.
//
// The first pass answered it with auditForeignTriggers, which refuses any
// row-level BEFORE INSERT trigger Wardyn does not ship and ERROR-logs the rest.
// It is keyed on the trigger's NAME being one of the three shipped ones — so the
// one shape it structurally cannot see is a trigger WEARING a shipped name:
//
//	DROP TRIGGER audit_events_chain ON audit_events;
//	CREATE TRIGGER audit_events_chain BEFORE INSERT ON audit_events
//	    FOR EACH ROW EXECUTE FUNCTION somebody_elses_function();
//
// auditTriggerNames sees the name and reports the trigger present; the foreign
// check excludes it by that same name; nothing compares it against what the
// migrations actually created. The result is the quiet bypass in its strongest
// form — every catalog check passes, the boot log is clean, and the trigger that
// the whole audit design rests on is the forger's.
//
// The append-only arm is the same hole with a worse ending: swap
// audit_events_no_update's function for one that returns NEW and UPDATE and
// DELETE are allowed on the audit log, while ensureAuditTriggers reports the
// append-only guarantee in force.
//
// Every subtest migrates into its OWN schema.

import (
	"context"
	"strings"
	"testing"
)

// TestPG_BootRepairsAChainTriggerBoundToAForeignFunction is the CHAIN arm. The
// remedy is a restore rather than a refusal, for the reason ensureAuditTriggers
// already states about a missing chain trigger: it is defined by idempotent
// DROP/CREATE migrations that can simply be replayed, and a wardynd that refuses
// to boot leaves the deployment with no audit log at all.
func TestPG_BootRepairsAChainTriggerBoundToAForeignFunction(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	for _, q := range []string{
		`CREATE FUNCTION impostor_chain() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN NEW.actor := 'someone-else@example.com'; NEW.outcome := 'success';
		       NEW.row_hash := repeat('a', 64); NEW.prev_hash := NULL; RETURN NEW; END; $$`,
		`DROP TRIGGER audit_events_chain ON audit_events`,
		`CREATE TRIGGER audit_events_chain BEFORE INSERT ON audit_events
		 FOR EACH ROW EXECUTE FUNCTION impostor_chain()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("install the impostor in %s: %v", schema, err)
		}
	}

	// The catalog is in the state the forger wants: the shipped NAME, present
	// and enabled, over a body nobody shipped.
	var proname string
	if err := pool.QueryRow(ctx,
		`SELECT p.proname FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
		  WHERE t.tgrelid = 'audit_events'::regclass AND t.tgname = $1`, auditChainTrigger).Scan(&proname); err != nil {
		t.Fatalf("read the trigger's function: %v", err)
	}
	if proname != "impostor_chain" {
		t.Fatalf("precondition: %s executes %q, want impostor_chain", auditChainTrigger, proname)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate over an impostor chain trigger: %v — the chain trigger is RESTORED, not refused, "+
			"because refusing leaves the deployment with no audit log at all", err)
	}

	if err := pool.QueryRow(ctx,
		`SELECT p.proname FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
		  WHERE t.tgrelid = 'audit_events'::regclass AND t.tgname = $1`, auditChainTrigger).Scan(&proname); err != nil {
		t.Fatalf("re-read the trigger's function: %v", err)
	}
	if proname != auditChainTrigger {
		t.Fatalf("%s still executes %q after Migrate, want the shipped %s — a trigger wearing a shipped name over a "+
			"foreign body passes every catalog check this boot makes, and it is the one that hash-chains every audit row",
			auditChainTrigger, proname, auditChainTrigger)
	}

	// And behaviourally: the row is stored as submitted, and chained.
	var actor, outcome, rowHash string
	if err := pool.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		VALUES (gen_random_uuid(), 'human', 'real@example.com', 'test.impostor.chain', 'denied')
		RETURNING actor, outcome, COALESCE(row_hash, '')`).Scan(&actor, &outcome, &rowHash); err != nil {
		t.Fatalf("append after the restore: %v", err)
	}
	if actor != "real@example.com" || outcome != "denied" {
		t.Errorf("stored actor/outcome = %q/%q, want real@example.com/denied — the impostor is still rewriting rows", actor, outcome)
	}
	if rowHash == "" {
		t.Error("row_hash is empty after the restore; the shipped chain trigger is not the one that ran")
	}
}

// TestPG_BootRefusesAnAppendOnlyTriggerBoundToAForeignFunction is the
// append-only arm, and it REFUSES for the reason ensureAuditTriggers already
// states for a missing one: those triggers are defined by 0001, the whole
// initial schema, and replaying that at boot to fix one trigger is a far bigger
// blast radius than refusing.
func TestPG_BootRefusesAnAppendOnlyTriggerBoundToAForeignFunction(t *testing.T) {
	ctx := context.Background()
	for _, name := range auditAppendOnlyTriggers {
		t.Run(name, func(t *testing.T) {
			pool, schema := probeSchemaPool(t)
			// Same event/level as the shipped trigger, so ONLY the function
			// differs — a shape check on tgtype alone cannot tell them apart.
			var timing string
			if name == "audit_events_no_update" {
				timing = "BEFORE UPDATE OR DELETE ON audit_events FOR EACH ROW"
			} else {
				timing = "BEFORE TRUNCATE ON audit_events FOR EACH STATEMENT"
			}
			for _, q := range []string{
				`CREATE FUNCTION impostor_permit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END; $$`,
				`DROP TRIGGER ` + name + ` ON audit_events`,
				`CREATE TRIGGER ` + name + ` ` + timing + ` EXECUTE FUNCTION impostor_permit()`,
			} {
				if _, err := pool.Exec(ctx, q); err != nil {
					t.Fatalf("install the impostor in %s: %v", schema, err)
				}
			}

			err := Migrate(ctx, pool)
			if err == nil {
				t.Fatalf("Migrate returned nil with %s bound to a function Wardyn does not ship — the append-only "+
					"guarantee is not in force, and the boot reported it as in force", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("Migrate error = %q; it must NAME the trigger so an operator knows what to repair", err)
			}
		})
	}
}
