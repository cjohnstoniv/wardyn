// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the review finding that the boot audit-trigger re-check reads the
// FULL trigger list on audit_events and then consults only the three names
// Wardyn ships, discarding the rest.
//
// The discarded rows are the quiet bypass the release documents twice
// (AuditDDLProtected's doc comment, docs/OPERATIONS.md): a row-level BEFORE
// INSERT trigger is handed NEW and whatever it returns is what Postgres stores.
// Measured on a throwaway database before this check existed, an event
// submitted through store.InsertAuditEvent as actor=X outcome=denied was stored
// as actor=Y outcome=success — InsertAuditEvent returned nil, Migrate returned
// nil, and store.VerifyAuditChain returned ok=true. The forgery is internally
// consistent, so the tamper-evidence control cannot see it.
//
// Every test here migrates into its OWN schema, so the lane's audit_events
// never carries a forging trigger.

import (
	"context"
	"strings"
	"testing"
)

func TestPG_BootRefusesAForeignBeforeInsertTriggerOnAuditEvents(t *testing.T) {
	ctx := context.Background()

	// Both name orders. The documented framing of this bypass is a trigger
	// sorting AFTER audit_events_chain, because same-event row triggers fire in
	// name order and the last one's NEW is what lands. That framing is too
	// narrow, and pinning only it would have left the EASIER attack open: a
	// trigger sorting BEFORE the chain trigger just rewrites NEW and lets the
	// SHIPPED chain trigger hash the forgery for it — no name trick, no hash
	// call. Both were executed against the unfixed tree; both left
	// VerifyAuditChain reporting ok=true.
	for _, name := range []string{"audit_events_zz_forge", "audit_events_aa_forge"} {
		t.Run(name, func(t *testing.T) {
			pool, schema := probeSchemaPool(t)
			for _, q := range []string{
				`CREATE FUNCTION audit_forge() RETURNS trigger LANGUAGE plpgsql AS $$
				 BEGIN NEW.outcome := 'success'; RETURN NEW; END; $$`,
				`CREATE TRIGGER ` + name + ` BEFORE INSERT ON audit_events
				 FOR EACH ROW EXECUTE FUNCTION audit_forge()`,
			} {
				if _, err := pool.Exec(ctx, q); err != nil {
					t.Fatalf("install forge in %s: %v", schema, err)
				}
			}

			err := Migrate(ctx, pool)
			if err == nil {
				t.Fatalf("Migrate returned nil with %s installed on audit_events — a row-level BEFORE INSERT trigger "+
					"rewrites the row on the way in, and the forgery it leaves is internally consistent, so every "+
					"shipped control (this boot check, the catalog check, the verify sweep) reports healthy", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("Migrate error = %q; it must NAME the trigger so an operator knows what to remove", err)
			}
		})
	}
}

// TestPG_BootAcceptsAForeignAfterInsertTrigger is the other direction: the
// refusal must not brick a deployment that legitimately hangs a replication or
// notify trigger off audit_events. An AFTER trigger cannot alter the stored
// row, so it is reported at ERROR rather than refused.
func TestPG_BootAcceptsAForeignAfterInsertTrigger(t *testing.T) {
	ctx := context.Background()
	pool, schema := probeSchemaPool(t)
	for _, q := range []string{
		`CREATE FUNCTION audit_notify() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RETURN NULL; END; $$`,
		`CREATE TRIGGER audit_events_zz_notify AFTER INSERT ON audit_events
		 FOR EACH ROW EXECUTE FUNCTION audit_notify()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("install notify trigger in %s: %v", schema, err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate refused over an AFTER INSERT trigger: %v — an AFTER trigger cannot rewrite the stored row, "+
			"so refusing would brick a deployment with a legitimate replication or notify trigger", err)
	}
	tamper, other, err := auditForeignTriggers(ctx, pool)
	if err != nil {
		t.Fatalf("auditForeignTriggers: %v", err)
	}
	if len(tamper) != 0 {
		t.Errorf("AFTER INSERT trigger classified as tamper-capable: %v", tamper)
	}
	if len(other) != 1 || other[0] != "audit_events_zz_notify" {
		t.Errorf("foreign AFTER trigger not reported: other = %v, want [audit_events_zz_notify]", other)
	}
}

// TestPG_BootAcceptsTheShippedTriggersAlone guards against the check firing on
// Wardyn's own triggers — the failure mode that would refuse every boot.
func TestPG_BootAcceptsTheShippedTriggersAlone(t *testing.T) {
	ctx := context.Background()
	pool, _ := probeSchemaPool(t)
	tamper, other, err := auditForeignTriggers(ctx, pool)
	if err != nil {
		t.Fatalf("auditForeignTriggers: %v", err)
	}
	if len(tamper) != 0 || len(other) != 0 {
		t.Errorf("a freshly migrated schema reports foreign triggers: tamperCapable=%v other=%v — "+
			"the shipped triggers must not be flagged as foreign", tamper, other)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate on a clean schema: %v", err)
	}
}
