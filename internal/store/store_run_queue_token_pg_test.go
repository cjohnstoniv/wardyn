// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run that waited in a pool's queue starts with a fresh token stamp and the minted token's id, not
// the stamp of its enqueue: a stale one lapses the run's runner lease and the lapsed-token sweep.
func TestStartQueuedRunStampsTheMintedToken(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)

	run, err := pg.CreateRun(ctx, newRun(types.RunPending))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET admitted_at = now(), token_renewed_at = now() - interval '2 hours' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	started, ok, err := pg.StartQueuedRun(ctx, run.ID, store.QueueStart{SPIFFEID: "spiffe://x/start", TokenJTI: "jti-start", ConfinementClass: types.CC2})
	if err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	renewed, err := pg.RunTokenRenewedAt(ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(renewed); age > time.Minute || age < -time.Minute {
		t.Errorf("token_renewed_at is %s old after the start, want a fresh stamp", age)
	}
	var jti string
	var prev *string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(token_jti, ''), token_prev_jti FROM agent_runs WHERE id = $1`, started.ID).Scan(&jti, &prev); err != nil {
		t.Fatal(err)
	}
	if jti != "jti-start" || prev != nil {
		t.Errorf("token_jti, token_prev_jti = %q, %v; want %q, nil", jti, prev, "jti-start")
	}
	// The proxy's first renew presents the minted token and must be accepted into that lineage.
	if stamped, err := pg.StampRunTokenRenewed(ctx, started.ID, "jti-renewed", "jti-start"); err != nil || !stamped {
		t.Errorf("first renew of the minted token = %v, %v; want stamped", stamped, err)
	}
}
