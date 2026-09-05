// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the review finding that the boot-time foreign-trigger refusal read
// pg_trigger.tgenabled through the SHIPPED-trigger filter (`IN ('O','A')`) and
// so could not see a foreign trigger armed with ENABLE REPLICA.
//
// 'R' is not an obscure state, it is the ONE the design already names as the
// bypass window: a session that sets session_replication_role = 'replica'
// silences every 'O' trigger and fires every 'R' one. So a forging row-level
// BEFORE INSERT trigger created and then ENABLE REPLICA'd is invisible to a
// boot check that only looks at 'O' and 'A', dormant for ordinary traffic, and
// armed for exactly the writer the audit design is trying to catch. With the
// shipped chain trigger hardened to 'A' (which docs/OPERATIONS.md recommends,
// and which makes it fire under replica too) the forgery is then hash-chained
// for free: actor and outcome are the forger's, row_hash is present and valid,
// and store.VerifyAuditChain reports the log clean.
//
// The existing pins in auditforeigntrigger_pg_test.go only ever install a
// trigger in its CREATE-TRIGGER default state, so 'A' and 'R' were untested as
// well as unhandled. This installs the SAME trigger in all three firing states
// and asserts one refusal for each.
//
// Every subtest migrates into its OWN schema, so the lane's audit_events never
// carries a forging trigger.

import (
	"context"
	"strings"
	"testing"
)

func TestPG_BootRefusesAForeignBeforeInsertTriggerInEveryFiringState(t *testing.T) {
	ctx := context.Background()

	// tgenabled, and the ALTER that produces it. 'O' is what CREATE TRIGGER
	// leaves behind; 'A' fires regardless of replication role; 'R' fires ONLY
	// under session_replication_role = replica.
	for _, tc := range []struct {
		state string
		alter string
	}{
		{"O", ""}, // control: already covered elsewhere, kept so a regression in the common case reddens here too
		{"A", `ALTER TABLE audit_events ENABLE ALWAYS TRIGGER adv3_forge`},
		{"R", `ALTER TABLE audit_events ENABLE REPLICA TRIGGER adv3_forge`},
	} {
		t.Run("tgenabled_"+tc.state, func(t *testing.T) {
			pool, schema := probeSchemaPool(t)
			for _, q := range []string{
				`CREATE FUNCTION adv3_forge_fn() RETURNS trigger LANGUAGE plpgsql AS $$
				 BEGIN NEW.actor := 'someone-else@example.com'; NEW.outcome := 'success'; RETURN NEW; END; $$`,
				`CREATE TRIGGER adv3_forge BEFORE INSERT ON audit_events
				 FOR EACH ROW EXECUTE FUNCTION adv3_forge_fn()`,
			} {
				if _, err := pool.Exec(ctx, q); err != nil {
					t.Fatalf("install forge in %s: %v", schema, err)
				}
			}
			if tc.alter != "" {
				if _, err := pool.Exec(ctx, tc.alter); err != nil {
					t.Fatalf("%s: %v", tc.alter, err)
				}
			}

			// The state the catalog actually holds, asserted rather than assumed:
			// an ALTER that silently did something else would make the rest of
			// this subtest a copy of the 'O' one.
			var got string
			if err := pool.QueryRow(ctx,
				`SELECT tgenabled::text FROM pg_trigger
				 WHERE tgrelid = 'audit_events'::regclass AND tgname = 'adv3_forge'`).Scan(&got); err != nil {
				t.Fatalf("read tgenabled: %v", err)
			}
			if got != tc.state {
				t.Fatalf("tgenabled = %q, want %q — the subtest is not testing the state it names", got, tc.state)
			}

			tamper, _, err := auditForeignTriggers(ctx, pool)
			if err != nil {
				t.Fatalf("auditForeignTriggers: %v", err)
			}
			if len(tamper) != 1 || tamper[0] != "adv3_forge" {
				t.Errorf("auditForeignTriggers tamperCapable = %v at tgenabled=%q, want [adv3_forge] — "+
					"a row-level BEFORE INSERT trigger rewrites NEW and the SHIPPED chain trigger then hashes the "+
					"forgery for it, in whatever state the forger armed it", tamper, tc.state)
			}

			err = Migrate(ctx, pool)
			if err == nil {
				t.Fatalf("Migrate returned nil with a foreign row-level BEFORE INSERT trigger at tgenabled=%q — "+
					"'R' is armed for exactly the session_replication_role = replica window the audit design treats "+
					"as the bypass to catch, and the row it forges is internally consistent, so the verify sweep "+
					"reports the log clean", tc.state)
			}
			if !strings.Contains(err.Error(), "adv3_forge") {
				t.Errorf("Migrate error = %q; it must NAME the trigger so an operator knows what to remove", err)
			}
		})
	}
}
