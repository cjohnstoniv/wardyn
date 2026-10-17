// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"hash/crc32"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_RecordingOutputRetentionExpiresWhileCommitWaits(t *testing.T) {
	for _, lock := range []string{"output_advisory", "mask_manifest", "output_row"} {
		t.Run(lock, func(t *testing.T) {
			pool := runsPGPoolIsolated(t)
			pg := store.NewPG(pool)
			run := persistRun(t, t.Context(), pool, newRun(types.RunCompleted))
			rp := newReplica(t, pool, localKEK(t))
			dispatched(t, rp, run.ID, run.CreatedBy)
			if lock == "output_row" {
				previous := queuedRecordingClaim(t, pg, run.ID)
				if wrote, err := pg.SaveRecordingRunOutput(t.Context(), previous, recordingRow(run.ID, "kept"), 0); err != nil || !wrote {
					t.Fatalf("seed previous recording: wrote=%v err=%v", wrote, err)
				}
			}
			claim := queuedRecordingClaim(t, pg, run.ID)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			blocker := testutil.PGConn(t, pool)
			hold, err := blocker.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = hold.Rollback(context.WithoutCancel(ctx)) }()
			var waitingQuery string
			switch lock {
			case "output_advisory":
				_, err = hold.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, db.RunOutputLockClass, int32(crc32.ChecksumIEEE(run.ID[:])))
				waitingQuery = "%pg_advisory_xact_lock%"
			case "mask_manifest":
				_, err = hold.Exec(ctx, `SELECT 1 FROM run_mask_manifest WHERE run_id=$1 FOR UPDATE`, run.ID)
				waitingQuery = "%FROM run_mask_manifest%FOR SHARE%"
			case "output_row":
				_, err = hold.Exec(ctx, `SELECT 1 FROM run_outputs WHERE run_id=$1 FOR UPDATE`, run.ID)
				waitingQuery = "%INSERT INTO run_outputs%"
			}
			if err != nil {
				t.Fatal(err)
			}
			// End time is fixed before Save starts. The database clock, not a
			// changed fixture or transaction timestamp, crosses the deadline.
			var cutoff time.Time
			if err := pool.QueryRow(ctx, `UPDATE agent_runs SET ended_at=clock_timestamp()-interval '1 hour'+interval '2 seconds'
				WHERE id=$1 RETURNING ended_at+interval '1 hour'`, run.ID).Scan(&cutoff); err != nil {
				t.Fatal(err)
			}
			type result struct {
				wrote bool
				err   error
			}
			done := make(chan result, 1)
			go func() {
				row := recordingRow(run.ID, "expired replacement")
				row.MaskScope = "run"
				wrote, err := pg.SaveRecordingRunOutput(ctx, claim, row, time.Hour)
				done <- result{wrote: wrote, err: err}
			}()
			var beforeCutoff bool
			waitFor(t, lock+" before retention expires", func() bool {
				var blocked bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					WHERE $1=ANY(pg_blocking_pids(pid)) AND query LIKE $2 AND xact_start<$3), clock_timestamp()<$3`,
					int32(blocker.PgConn().PID()), waitingQuery, cutoff).Scan(&blocked, &beforeCutoff); err != nil {
					t.Fatal(err)
				}
				return blocked
			})
			if !beforeCutoff {
				t.Fatal("save did not reach its blocking lock before the cutoff")
			}
			waitFor(t, "the actual database retention cutoff", func() bool {
				var expired bool
				if err := pool.QueryRow(ctx, `SELECT clock_timestamp()>$1`, cutoff).Scan(&expired); err != nil {
					t.Fatal(err)
				}
				return expired
			})
			if err := hold.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if got.err != nil || got.wrote {
					t.Fatalf("post-retention commit wrote=%v err=%v", got.wrote, got.err)
				}
			case <-ctx.Done():
				t.Fatalf("save remained blocked: %v", ctx.Err())
			}
			row, found, err := pg.GetRunOutput(ctx, run.ID)
			if err != nil || lock == "output_row" && (!found || string(row.Output) != "kept") || lock != "output_row" && found {
				t.Fatalf("expired recovery changed stored output: %+v found=%v err=%v", row, found, err)
			}
			var retired bool
			if err := pool.QueryRow(ctx, `SELECT completed_generation=requested_generation AND claim_token IS NULL
				FROM run_output_recording_recovery WHERE run_id=$1`, run.ID).Scan(&retired); err != nil || !retired {
				t.Fatalf("expired obligation can starve retries: retired=%v err=%v", retired, err)
			}
			if ids, err := pg.ListPendingRecordingRunOutputs(ctx, time.Minute, 10); err != nil || len(ids) != 0 {
				t.Fatalf("expired work still occupies the backlog: %v %v", ids, err)
			}
		})
	}
}
