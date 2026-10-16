// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// setRecords sets whether the runner records a run on its organisation's word.
func (e *placementEnv) setRecords(id uuid.UUID, on bool) {
	f := e.runner.runnerFacts[id.String()]
	f.Records = on
	e.runner.runnerFacts[id.String()] = f
}

// TestPG_RecordingRequired_RefusesARunnerThatDoesNotRecord: under the default (preferred) a run is
// placed on a runner that does not record; once the organisation or the pool requires recording it
// is refused runner_recording_off with the pool's sentence, and a runner that records is placed.
func TestPG_RecordingRequired_RefusesARunnerThatDoesNotRecord(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	id := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, laptops.ID, id)
	ctx := operatorCtx(e.personP, "person@corp.example", oidc.RoleUser)
	ceiling, err := e.srv.effectiveCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := createRunRequest{Agent: "claude-code", Placement: types.PlacementLocal, RunnerPoolID: laptops.ID.String()}
	resolve := func() *runRefusal {
		_, r := e.srv.resolvePlacement(ctx, placementInput{req: &req, reqCCOK: true, ceiling: ceiling})
		return r
	}
	refusedOff := func(when string) {
		t.Helper()
		r := resolve()
		if r == nil || r.status != http.StatusUnprocessableEntity || r.body.Reason != string(placement.ReasonRunnerRecordingOff) ||
			r.body.Error != runnerpool.RunnerRecordingOffMsg("Laptops") {
			t.Fatalf("%s: refusal = %+v, want 422 runner_recording_off", when, r)
		}
	}
	if r := resolve(); r != nil {
		t.Fatalf("preferred: a runner that does not record was refused: %+v", r.body)
	}

	org, err := e.pg.GetRunnerPoolOrgDefaults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	org.Recording = types.RecordingRequired
	if err := e.pg.PutRunnerPoolOrgDefaults(context.Background(), org, "admin"); err != nil {
		t.Fatal(err)
	}
	refusedOff("organisation requires")
	e.setRecords(id, true)
	if r := resolve(); r != nil {
		t.Fatalf("organisation requires, runner records: %+v", r.body)
	}

	org.Recording = ""
	if err := e.pg.PutRunnerPoolOrgDefaults(context.Background(), org, "admin"); err != nil {
		t.Fatal(err)
	}
	e.setRecords(id, false)
	w := e.call(e.person, http.MethodPut, "/api/v1/me/runner-pools/"+laptops.ID.String(),
		fmt.Sprintf(`{"revision":%d,"recording":"required"}`, e.revision(laptops.ID)))
	if w.Code != http.StatusOK || decodeJSON[types.RunnerPool](t, w).Recording != types.RecordingRequired {
		t.Fatalf("set the pool's recording = %d %s", w.Code, w.Body)
	}
	refusedOff("pool requires")
	if w := e.call(e.person, http.MethodPut, "/api/v1/me/runner-pools/"+laptops.ID.String(),
		fmt.Sprintf(`{"revision":%d,"recording":"always"}`, e.revision(laptops.ID))); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown recording setting = %d %s, want 400", w.Code, w.Body)
	}
}

// TestPG_RunSaysWhyItIsNotRecorded: dispatch asks the runner to record exactly when the organisation
// keeps recordings, and the run row keeps why it is not recorded.
func TestPG_RunSaysWhyItIsNotRecorded(t *testing.T) {
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		store   recording.Store
		records bool
		want    types.RunNotRecorded
	}{
		{"organisation off", nil, true, types.NotRecordedOrgOff},
		{"runner declines", fs, false, types.NotRecordedRunnerDeclined},
		{"recorded", fs, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			e.srv.cfg.RecordingStore = tc.store
			laptops := e.createPool("Laptops", types.RunnerPoolUser)
			id := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
			e.setRecords(id, tc.records)
			e.addToPool(e.person, laptops.ID, id)
			e.stream(id, types.CC1, types.CC2, types.CC3)
			e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, laptops.ID))

			run := e.localCreate(runBody(""))
			spec := e.dispatchedSpec(run.ID)
			if spec.Record == nil || *spec.Record != (tc.store != nil) {
				t.Fatalf("dispatched record = %v, want %v", spec.Record, tc.store != nil)
			}
			got, err := e.pg.GetRun(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.NotRecorded != tc.want {
				t.Fatalf("not_recorded = %q, want %q", got.NotRecorded, tc.want)
			}
		})
	}
}
