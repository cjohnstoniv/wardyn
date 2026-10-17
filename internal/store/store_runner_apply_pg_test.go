// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner's first success receipt for a queued kill is audited as runner.action.apply in the
// receipt's own transaction: the server's record that the run is contained. A repeat adds none.
func TestPG_RunnerActions_SuccessIsAuditedAsApplied(t *testing.T) {
	f := newActionFixture(t)
	ctx := t.Context()
	run := f.run(t)
	a := f.mustQueue(t, f.action(run, types.RunnerActionKill, uuid.Nil))
	observed := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	for range 2 {
		if _, _, err := f.st.RecordRunnerActionResult(ctx, f.rn.ID, run.ID, a.ID, types.RunnerActionSuccess, "", observed, prepareSpoolTest); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'runner.action.apply' AND outcome = 'success' AND run_id = $1
		AND target = $2 AND actor = $3 AND data->>'action' = 'kill' AND data->>'run_id' = $1::text`, run.ID, a.ID.String(), "runner:"+f.rn.ID.String()); n != 1 {
		t.Fatalf("%d runner.action.apply rows, want one", n)
	}
}
