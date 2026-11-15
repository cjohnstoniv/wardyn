// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// changedDuringRecord is the store whose first record of a workload's new run waits until apply, a changed
// apply of the same workload, has answered: a launch of the older spec still starting when the newer one is
// applied.
type changedDuringRecord struct {
	store.PG
	fired *atomic.Bool
	apply func()
}

func (s changedDuringRecord) SetWorkloadRun(ctx context.Context, id, claim uuid.UUID, generation int64, runID uuid.UUID) (*uuid.UUID, error) {
	if s.fired.CompareAndSwap(false, true) {
		s.apply()
	}
	return s.PG.SetWorkloadRun(ctx, id, claim, generation, runID)
}

// A changed apply made while the launch of the first spec is still starting launches the newer spec: its run
// becomes the workload's at generation 2, and the older launch's run, which the workload never took, is ended
// workload_replaced. The older apply answers that it launched nothing.
func TestPG_Workload_AChangedApplyDuringALaunchLaunchesTheNewerSpec(t *testing.T) {
	e := newSvcEnv(t)
	pol := e.plainPolicy(t, "wl-pol")
	before := agentRunIDs(t, e)
	var newer client.WorkloadApplyResult
	e.h.srv.cfg.Store = changedDuringRecord{PG: e.st, fired: new(atomic.Bool), apply: func() {
		newer = decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t2", "")), http.StatusOK)
	}}
	older := decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated)
	e.h.srv.cfg.Store = e.st
	newRun := launchedRun(t, newer)
	if older.Launch.State != client.WorkloadNoLaunch {
		t.Fatalf("older apply's launch = %+v, want none: a newer spec replaced it while it started", older.Launch)
	}
	v, err := e.st.GetWorkload(context.Background(), "mem", "web")
	if err != nil || v.Generation != 2 || v.CurrentGeneration != 2 || v.CurrentRunID == nil || *v.CurrentRunID != newRun {
		t.Fatalf("workload = %+v (%v), want generation 2 current with run %s", v.Workload, err, newRun)
	}
	wantRunLive(t, e, newRun, "the newer spec's run")
	var untaken []uuid.UUID
	for id := range agentRunIDs(t, e) {
		if !before[id] && id != newRun {
			untaken = append(untaken, id)
		}
	}
	if len(untaken) != 1 {
		t.Fatalf("runs the workload did not take = %v, want the older launch's one", untaken)
	}
	wantKilledFor(t, e, untaken[0], workloadKillReplaced)
}
