// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"regexp"
	"testing"
)

// TestPG_TriggerReplayKeepsV2Chain_PG: a boot-time restore of the chain trigger replays every migration that
// defines it, in name order, so the file that sorts last decides which chain body the database is left with.
// That file is the format 2 one, and after a replay a new row is salted and hashed by it.
func TestPG_TriggerReplayKeepsV2Chain_PG(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	names, err := triggerMigrationFiles(auditChainTrigger)
	if err != nil {
		t.Fatalf("triggerMigrationFiles: %v", err)
	}
	if last := names[len(names)-1]; last != auditChainLastFile {
		t.Fatalf("the replay set ends with %s, want %s", last, auditChainLastFile)
	}
	if err := replayTriggerMigrations(ctx, pool, auditChainTrigger); err != nil {
		t.Fatalf("replay the chain trigger's migrations: %v", err)
	}

	var seq int64
	if err := pool.QueryRow(ctx, `SELECT seq FROM audit_append(gen_random_uuid(), now(), NULL, 'system', 'replay-probe', 'test.replay.v2', '', 'success', '', NULL)`).Scan(&seq); err != nil {
		t.Fatalf("append after the replay in %s: %v", schema, err)
	}
	var format int16
	var salt, stored, viaAny string
	if err := pool.QueryRow(ctx, `
		SELECT e.hash_format, e.hash_salt, e.row_hash,
		       audit_row_hash_any(e.hash_format, e.hash_salt, e.prev_hash, e.id, e.time, e.run_id, e.actor_type, e.actor,
		                          e.action, e.target, e.outcome, e.source_ip, e.data)
		  FROM audit_events e WHERE e.seq = $1`, seq).Scan(&format, &salt, &stored, &viaAny); err != nil {
		t.Fatalf("read the appended row: %v", err)
	}
	if format != 2 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(salt) {
		t.Errorf("a row appended after the replay has format %d and a salt of %d characters, want 2 and 64 hex", format, len(salt))
	}
	if stored != viaAny {
		t.Errorf("the stored row_hash is not audit_row_hash_any over the row: the replay left a body that hashes another way")
	}
}
