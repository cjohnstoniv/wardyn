// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// TestThreeReplicasGoneWithinOneCycle_PG: deterministic claiming across three replicas. With three replicas on one
// database, a connected runner's run whose container was reported absent over 30 seconds ago, and whose watcher
// lease is stale, ends FAILED (gone) in one sweep by each replica, whichever place the holder of the runner's session
// takes in that round. The other two replicas have peering and a holder row that routes the runner to the holder, and
// still neither claim the run nor check its sandbox, before the holder's sweep or after it; the run has one run.lost
// row. It starts past the first absent report and its confirmation, so it does not measure the time from the
// container's removal to the run's end.
func TestThreeReplicasGoneWithinOneCycle_PG(t *testing.T) {
	for holderAt := range 3 {
		t.Run(fmt.Sprintf("the holder sweeps %d of 3", holderAt+1), func(t *testing.T) {
			g := goneProbeFixture(t, t.Context())
			ctx := t.Context()
			if err := g.st.SetRunAgentExecID(ctx, g.run.ID, "exec-1"); err != nil {
				t.Fatal(err)
			}
			// The container is gone: a check found it absent over 30 s ago, and the run's watcher lease is stale.
			g.exec(`UPDATE agent_runs SET sandbox_absent_at=now()-interval '31 seconds', watcher_heartbeat=now()-interval '1 hour' WHERE id=$1`)
			g.holdSession()
			var session uuid.UUID
			if err := g.pool.QueryRow(ctx, `SELECT session_id FROM runner_inventories WHERE runner_id=$1`, g.rn.ID).Scan(&session); err != nil {
				t.Fatal(err)
			}
			if ok, err := g.st.ClaimRunnerSessionHolder(ctx, g.rn.ID, session, "holder", "https://holder.invalid"); err != nil || !ok {
				t.Fatalf("the holder row for session %s: wrote=%v err=%v", session, ok, err)
			}
			var replicas []*Server
			for range 2 {
				cfg := g.cfg
				peerShape(newPeerConfig())(&cfg)
				replicas = append(replicas, New(cfg))
			}
			replicas = slices.Insert(replicas, holderAt, g.srv)
			checks := 0
			for i, srv := range replicas {
				if err := srv.sweepRunWatchers(ctx); err != nil {
					t.Fatalf("sweep %d: %v", i+1, err)
				}
				switch n := g.gp.calls(); {
				case i < holderAt && n != 0:
					t.Fatalf("sweep %d, before the holder's: %d status checks of the run's sandbox, want 0", i+1, n)
				case i == holderAt:
					checks = n
				case i > holderAt && n != checks:
					t.Fatalf("sweep %d, after the holder's: %d status checks of the run's sandbox, want %d", i+1, n, checks)
				}
			}
			g.checkGoneEnd(g.getRun())
		})
	}
}
