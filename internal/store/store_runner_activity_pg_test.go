// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

func TestPG_RunnerCurrentActivityGuardsAndOrgClock(t *testing.T) {
	for _, scenario := range []string{"current", "lapsed", "kept", "killed", "ended", "wrong-owner", "revoked"} {
		t.Run(scenario, func(t *testing.T) {
			pool, st, rn, run, _ := spoolPGFixture(t)
			ctx := context.Background()
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET updated_at=now()-interval '1 day',active_at=now()-interval '1 day',token_renewed_at=now() WHERE id=$1`, run.ID); err != nil {
				t.Fatal(err)
			}
			query := ""
			switch scenario {
			case "lapsed":
				query = `UPDATE agent_runs SET token_renewed_at=now()-interval '66 minutes' WHERE id=$1`
			case "kept":
				query = `UPDATE agent_runs SET lost_at=now(),lost_reason='outage' WHERE id=$1`
			case "killed":
				query = `UPDATE agent_runs SET state='KILLED' WHERE id=$1`
			case "ended":
				query = `UPDATE agent_runs SET ends_at=now()-interval '1 second' WHERE id=$1`
			case "wrong-owner":
				query = `UPDATE agent_runs SET created_by='other' WHERE id=$1`
			case "revoked":
				if _, err := pool.Exec(ctx, `UPDATE runners SET revoked_at=now(),state='revoked' WHERE id=$1`, rn.ID); err != nil {
					t.Fatal(err)
				}
			}
			if query != "" {
				if _, err := pool.Exec(ctx, query, run.ID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := st.GetRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			err = st.TouchRunnerActivity(ctx, rn.ID, run.ID)
			after, getErr := st.GetRun(ctx, run.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if scenario != "current" {
				if !errors.Is(err, store.ErrRunnerExecutionRefused) {
					t.Fatalf("activity accepted %v", err)
				}
				if !after.UpdatedAt.Equal(before.UpdatedAt) {
					t.Fatal("refused activity moved idle clock")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !after.UpdatedAt.After(before.UpdatedAt) || time.Since(after.UpdatedAt) > time.Minute {
				t.Fatal("activity did not use current receipt clock")
			}
			var events int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1`, run.ID).Scan(&events); err != nil || events != 0 {
				t.Fatalf("activity authored audit %d %v", events, err)
			}
		})
	}
}
