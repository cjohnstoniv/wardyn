// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A pass back's send-time bound against a real Postgres, whose age read answers only for the generation the record
// holds (store.FolderGenAges). Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testfloor"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pgUnansweredPassRun is a COMPLETED run on Postgres with a seeded both-ways folder on a laptop runner that is not
// connected here. Its record is at gen 2 with gen 1 settled, so its last pass is unanswered, kept with stalled
// stalls, and sent an hour past oldPassBound on the database's clock.
func pgUnansweredPassRun(t *testing.T, e *poolEnv, stalled int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	laptop := uuid.New()
	run := e.boundRun(func(r *types.AgentRun) {
		r.State, r.SandboxRef = types.RunCompleted, "sbx-"+r.ID.String()
		r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &laptop, Path: "/Users/alice/code/my-app", Direction: types.ComputerFolderBoth}
	})
	if ok, err := e.pg.SetRunLocalFolderSeed(ctx, run, ""); !ok || err != nil {
		t.Fatalf("seed: %v %v", ok, err)
	}
	if ok, err := e.pg.SetRunLocalFolderStatusIf(ctx, run, store.FolderStatusGuard{}, types.RunLocalFolderStatus{
		Settled: 1, Resume: &types.FolderResume{Stalled: stalled, Unsynced: 1},
	}); !ok || err != nil {
		t.Fatalf("record: %v %v", ok, err)
	}
	if _, err := e.pg.Pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = local_folder_status
		|| jsonb_build_object('gen', 2, 'gen_at', now() - make_interval(secs => $2)) WHERE id = $1`,
		run, (oldPassBound + time.Hour).Seconds()); err != nil {
		t.Fatal(err)
	}
	return run
}

// On Postgres the bound is read before the pass back's own bump moves the record to gen 3. A last pass unanswered
// past its bound that was a stall in a row of them gives the changes up at the visit, with no pass. One that was
// not gets its pass sent: here to a runner that is not connected, whose refusal is the pass's answer, so the new
// generation is settled. Catches: the bound asked after the bump with the generation it moved from, which Postgres
// always answers "not past", so the give-up never comes and no pass is sent again until the 7-day cap.
func TestPG_PassBack_TheBoundIsReadBeforeTheBump(t *testing.T) {
	testfloor.Mark(t, "pg")
	e := newPoolEnv(t, false)
	ctx := context.Background()
	e.srv.cfg.Runner = &fakeRunner{}
	e.srv.cfg.ComputerFolders = true

	given := pgUnansweredPassRun(t, e, computerFolderStalls)
	if e.srv.bringComputerFolderBack(ctx, given, "sbx-"+given.String()) {
		t.Error("a stall past its bound kept the sandbox")
	}
	st, err := e.pg.RunLocalFolderStatus(ctx, given)
	if err != nil || st.Gen != 3 || st.Settled != 3 || st.Resume != nil || st.Back == nil ||
		!strings.Contains(st.Back.Error, "because 2 passes in a row brought nothing back: the runner did not answer for its last pass back") {
		t.Errorf("stall past the bound: record %+v (back %+v), %v; want the give-up at gen 3, settled, no resume", st, st.Back, err)
	}

	sent := pgUnansweredPassRun(t, e, 1)
	e.srv.bringComputerFolderBack(ctx, sent, "sbx-"+sent.String())
	st, err = e.pg.RunLocalFolderStatus(ctx, sent)
	if err != nil || st.Gen != 3 || st.Settled != 3 || st.Back == nil || strings.Contains(st.Back.Error, "may still be running its last pass") {
		t.Errorf("one stall past the bound: record %+v (back %+v), %v; want the pass sent at gen 3 and settled by the runner's answer", st, st.Back, err)
	}
}
