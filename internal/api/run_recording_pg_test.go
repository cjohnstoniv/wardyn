// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
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
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.RecordingStore = fs
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
	if _, _, err := e.pg.PutRunnerPoolOrgDefaults(context.Background(), org, "admin"); err != nil {
		t.Fatal(err)
	}
	refusedOff("organisation requires")
	e.setRecords(id, true)
	if r := resolve(); r != nil {
		t.Fatalf("organisation requires, runner records: %+v", r.body)
	}

	org.Recording = types.RecordingPreferred
	if _, _, err := e.pg.PutRunnerPoolOrgDefaults(context.Background(), org, "admin"); err != nil {
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

// requireRecording writes the organisation's recording setting straight to the store, past the API's
// refusal to require recording without a store.
func (e *placementEnv) requireRecording(rec types.RecordingRequirement) {
	e.t.Helper()
	org, err := e.pg.GetRunnerPoolOrgDefaults(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	org.Recording = rec
	if _, _, err := e.pg.PutRunnerPoolOrgDefaults(context.Background(), org, "admin"); err != nil {
		e.t.Fatal(err)
	}
}

// wantRefusal fails unless r is a 422 with reason and sentence msg.
func wantRefusal(t *testing.T, when string, r *runRefusal, reason placement.Reason, msg string) {
	t.Helper()
	if r == nil || r.status != http.StatusUnprocessableEntity || r.body.Reason != string(reason) || r.body.Error != msg {
		t.Fatalf("%s: refusal = %+v, want 422 %s %q", when, r, reason, msg)
	}
}

// TestPG_RecordingRequired_NeedsTheOrganisationsStore: with no recording store, a required setting is
// refused when saved (the organisation's defaults, an organisation pool, a person's pool), and a run a
// required setting holds is refused recording_store_off at create.
func TestPG_RecordingRequired_NeedsTheOrganisationsStore(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	wantSaveRefused := func(when string, code int, body string) {
		t.Helper()
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"reason":"recording_store_off"`) ||
			!strings.Contains(body, runnerpool.RecordingSaveStoreOffMsg()) {
			t.Fatalf("%s = %d %s, want 422 recording_store_off", when, code, body)
		}
	}
	w := e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", `{"recording":"required"}`)
	wantSaveRefused("save the organisation's required", w.Code, w.Body.String())
	w = e.admin(http.MethodPost, "/api/v1/runner-pools", `{"name":"Recorded","recording":"required"}`)
	wantSaveRefused("create a required organisation pool", w.Code, w.Body.String())
	w = e.call(e.person, http.MethodPut, "/api/v1/me/runner-pools/"+laptops.ID.String(),
		fmt.Sprintf(`{"revision":%d,"recording":"required"}`, e.revision(laptops.ID)))
	wantSaveRefused("require recording on a person's pool", w.Code, w.Body.String())
	if w := e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", `{"recording":"preferred"}`); w.Code != http.StatusOK {
		t.Fatalf("save the organisation's preferred = %d %s", w.Code, w.Body)
	}

	id := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.setRecords(id, true)
	e.addToPool(e.person, laptops.ID, id)
	ctx := operatorCtx(e.personP, "person@corp.example", oidc.RoleUser)
	ceiling, err := e.srv.effectiveCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := createRunRequest{Agent: "claude-code", Placement: types.PlacementLocal, RunnerPoolID: laptops.ID.String()}
	e.requireRecording(types.RecordingRequired)
	_, r := e.srv.resolvePlacement(ctx, placementInput{req: &req, reqCCOK: true, ceiling: ceiling})
	wantRefusal(t, "a pooled run, no store", r, placement.ReasonRecordingStoreOff, runnerpool.RecordingStoreOffMsg())
}

// TestPG_RecordingRequired_HoldsARunWithNoPool: the organisation's required setting reaches a run
// that has no pool, at create (a deployment whose runs pools do not place) and at dispatch.
func TestPG_RecordingRequired_HoldsARunWithNoPool(t *testing.T) {
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := newPlacementEnv(t)
	id := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	ctx := operatorCtx(e.personP, "person@corp.example", oidc.RoleUser)
	ceiling, err := e.srv.effectiveCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("create", func(t *testing.T) {
		wired := e.srv.cfg.Runner
		e.srv.cfg.Runner = nil // pools place no run: every run is unpooled
		t.Cleanup(func() {
			e.srv.cfg.Runner, e.srv.cfg.RecordingStore = wired, nil
			e.requireRecording(types.RecordingPreferred)
		})
		local := createRunRequest{Agent: "claude-code", Placement: types.PlacementLocal, RunnerID: id.String()}
		resolve := func(req createRunRequest) *runRefusal {
			_, r := e.srv.resolvePlacement(ctx, placementInput{req: &req, reqCCOK: true, ceiling: ceiling})
			return r
		}
		e.srv.cfg.RecordingStore = fs
		if r := resolve(local); r != nil && r.body.Reason == string(placement.ReasonRunnerRecordingOff) {
			t.Fatalf("preferred: refused %+v", r.body)
		}
		e.requireRecording(types.RecordingRequired)
		wantRefusal(t, "required, the runner does not record", resolve(local), placement.ReasonRunnerRecordingOff, runnerpool.RunnerRecordingOffUnpooledMsg())
		if r := resolve(createRunRequest{Agent: "claude-code"}); r != nil && (r.body.Reason == string(placement.ReasonRunnerRecordingOff) ||
			r.body.Reason == string(placement.ReasonRecordingStoreOff)) {
			t.Fatalf("required, a headless remote run (never placed): refused %+v", r.body)
		}
		e.srv.cfg.RecordingStore = nil
		wantRefusal(t, "required, no store", resolve(local), placement.ReasonRecordingStoreOff, runnerpool.RecordingStoreOffMsg())
	})

	t.Run("dispatch", func(t *testing.T) {
		starting := func() types.AgentRun {
			t.Helper()
			rid, at := uuid.New(), time.Now().UTC()
			run, err := e.pg.CreateRun(context.Background(), types.AgentRun{ID: rid, CreatedAt: at, UpdatedAt: at, CreatedBy: e.personP, Agent: "claude-code",
				ConfinementClass: types.CC2, State: types.RunStarting, SPIFFEID: "spiffe://wardyn.local/agent-run/" + rid.String(), RunnerTarget: "docker",
				Placement: types.PlacementLocal, EvidenceSource: types.RunEvidenceRunnerAsserted, RunnerID: &id})
			if err != nil {
				t.Fatal(err)
			}
			return run
		}
		for _, tc := range []struct {
			name     string
			store    recording.Store
			required bool
			records  bool
			reason   placement.Reason
			msg      string
		}{
			{"preferred, runner declines", fs, false, false, "", ""},
			{"required, runner records", fs, true, true, "", ""},
			{"required, runner declines", fs, true, false, placement.ReasonRunnerRecordingOff, runnerpool.RunnerRecordingOffUnpooledMsg()},
			{"required, no store", nil, true, true, placement.ReasonRecordingStoreOff, runnerpool.RecordingStoreOffMsg()},
			{"required, runner facts unknown", fs, true, false, "", ""},
		} {
			e.srv.cfg.RecordingStore = tc.store
			e.setRecords(id, tc.records)
			if tc.name == "required, runner facts unknown" {
				delete(e.runner.runnerFacts, id.String())
			}
			if tc.required {
				e.requireRecording(types.RecordingRequired)
			} else {
				e.requireRecording(types.RecordingPreferred)
			}
			run := starting()
			var spec runner.SandboxSpec
			ok := e.srv.applyRunRecording(context.Background(), &run, &spec)
			got, err := e.pg.GetRun(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.reason == "" {
				if !ok || got.State != types.RunStarting {
					t.Fatalf("%s: dispatch refused (state %s)", tc.name, got.State)
				}
				continue
			}
			var reason any
			for _, ev := range e.h.audit.snapshot() {
				if ev.Action == "run.create" && ev.RunID != nil && *ev.RunID == run.ID {
					reason = auditData(t, ev)["reason"]
				}
			}
			if ok || got.State != types.RunFailed || got.FailureHint != tc.msg || reason != string(tc.reason) {
				t.Fatalf("%s: ok=%v state=%s hint=%q audit reason=%v, want failed %s", tc.name, ok, got.State, got.FailureHint, reason, tc.reason)
			}
		}
	})
}

// TestPG_QueuedRunWaitsForARecordingRunner: under required, a queued run whose only free runner does not
// record keeps its place (runner_pool_no_runner_free) instead of being promoted to fail, and starts once
// the runner records.
func TestPG_QueuedRunWaitsForARecordingRunner(t *testing.T) {
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := newQueueEnv(t)
	e.srv.cfg.RecordingStore = fs
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(nil, pool, "")
	e.requireRecording(types.RecordingRequired)
	e.free(occupant)
	e.sweep()
	if got := e.run(waiting.ID); got.State != types.RunQueued || got.StatusDetail != "queue: runner_pool_no_runner_free" || e.queueRows(waiting.ID) != 1 {
		t.Fatalf("with only a non-recording runner free the run is %s %q with %d rows; want QUEUED waiting with runner_pool_no_runner_free",
			got.State, got.StatusDetail, e.queueRows(waiting.ID))
	}
	f := e.runner.execFacts["alpha"]
	f.Records = true
	e.runner.execFacts["alpha"] = f
	e.sweep()
	e.waitStarted(waiting.ID)
}

// TestPG_DefaultsWriteWithoutRecordingKeepsIt: a write of the organisation's defaults that leaves
// recording out (the console's Make default, a CLI older than recording) keeps required; only a
// write that names recording changes it. A pool update that leaves recording out keeps its own too.
func TestPG_DefaultsWriteWithoutRecordingKeepsIt(t *testing.T) {
	e := newPlacementEnv(t)
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.RecordingStore = fs
	ci := e.createPool("CI jobs", types.RunnerPoolOrganization)
	stored := func(when string, want types.RecordingRequirement) {
		t.Helper()
		d, err := e.pg.GetRunnerPoolOrgDefaults(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if d.Recording != want {
			t.Fatalf("%s: stored recording = %q, want %q", when, d.Recording, want)
		}
	}
	put := func(when, body string) {
		t.Helper()
		if w := e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", body); w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", when, w.Code, w.Body)
		}
	}
	put("require recording", `{"recording":"required"}`)
	put("make a pool the default", fmt.Sprintf(`{"pool_id":%q}`, ci.ID))
	stored("make a pool the default", types.RecordingRequired)
	put("clear the default", `{}`)
	stored("clear the default", types.RecordingRequired)
	put("lower recording", `{"recording":"preferred"}`)
	stored("lower recording", types.RecordingPreferred)
	put("require recording again", `{"recording":"required"}`)
	put("lower recording with an empty value", `{"recording":""}`)
	stored("lower recording with an empty value", types.RecordingPreferred)

	// The keep is the store's, against the row it writes, not a read the handler made earlier.
	put("require recording again", `{"recording":"required"}`)
	_, got, err := e.pg.PutRunnerPoolOrgDefaults(context.Background(), types.RunnerPoolDefaults{}, "admin")
	if err != nil || got.Recording != types.RecordingRequired {
		t.Fatalf("store write without recording = %+v, %v; want required kept", got, err)
	}
	stored("store write without recording", types.RecordingRequired)

	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+ci.ID.String(),
		fmt.Sprintf(`{"revision":%d,"recording":"required"}`, e.revision(ci.ID))); w.Code != http.StatusOK {
		t.Fatalf("require recording on the pool = %d %s", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+ci.ID.String(),
		fmt.Sprintf(`{"revision":%d,"name":"CI"}`, e.revision(ci.ID))); w.Code != http.StatusOK {
		t.Fatalf("rename the pool = %d %s", w.Code, w.Body)
	}
	if p, err := e.pg.GetRunnerPool(context.Background(), ci.ID); err != nil || p.Recording != types.RecordingRequired {
		t.Fatalf("pool after a rename = %+v, %v; want recording required", p.Recording, err)
	}
}

// TestPG_QueuedRunWaitsForTheRecordingStore: under required, a queued run in an organisation that keeps
// no session recordings says so (runner_pool_recording_store_off) rather than that no runner is online,
// keeps its place, and starts once the store is back.
func TestPG_QueuedRunWaitsForTheRecordingStore(t *testing.T) {
	fs, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := newQueueEnv(t)
	e.srv.cfg.RecordingStore = fs
	f := e.runner.execFacts["alpha"]
	f.Records = true
	e.runner.execFacts["alpha"] = f
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	waiting := e.queuedLaunch(nil, pool, "")
	e.requireRecording(types.RecordingRequired)
	e.srv.cfg.RecordingStore = nil
	e.free(occupant)
	e.sweep()
	if got := e.run(waiting.ID); got.State != types.RunQueued || got.StatusDetail != "queue: runner_pool_recording_store_off" || e.queueRows(waiting.ID) != 1 {
		t.Fatalf("with no recording store the run is %s %q with %d rows; want QUEUED waiting with runner_pool_recording_store_off",
			got.State, got.StatusDetail, e.queueRows(waiting.ID))
	}
	e.srv.cfg.RecordingStore = fs
	e.sweep()
	e.waitStarted(waiting.ID)
}
