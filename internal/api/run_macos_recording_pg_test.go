// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_MacOSVMRecordingRequiredStarts: where the organisation requires recording, a macOS VM run is no
// longer refused for it at the pool, on a runner that does not record either (the organisation records a
// macOS VM run, so no runner is asked to), while a container in the same pool on the same runner is still
// refused runner_recording_off.
func TestPG_MacOSVMRecordingRequiredStarts(t *testing.T) {
	e := newPlacementEnv(t)
	e.srv.cfg.RecordingStore = fsRecordingStore(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	id := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.setRecords(id, false)
	e.addToPool(e.person, laptops.ID, id)
	ctx := operatorCtx(e.personP, "person@corp.example", oidc.RoleUser)
	ceiling, err := e.srv.effectiveCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(kind string) *runRefusal {
		req := createRunRequest{Agent: "claude-code", Placement: types.PlacementLocal, RunnerPoolID: laptops.ID.String(), MachineKind: kind}
		_, r := e.srv.resolvePlacement(ctx, placementInput{req: &req, reqCCOK: true, ceiling: ceiling})
		return r
	}
	forRecording := func(r *runRefusal) bool {
		return r != nil && (r.body.Reason == string(placement.ReasonRunnerRecordingOff) || r.body.Reason == string(placement.ReasonRecordingStoreOff) ||
			strings.Contains(r.body.Error, "macOS VM run"))
	}

	e.requireRecording(types.RecordingRequired)
	if r := resolve("macos_vm"); forRecording(r) {
		t.Fatalf("required, a macOS VM on a runner that does not record: refused for recording: %d %+v", r.status, r.body)
	}
	r := resolve("")
	if r == nil || r.status != http.StatusUnprocessableEntity || r.body.Reason != string(placement.ReasonRunnerRecordingOff) {
		t.Fatalf("required, a container on the same runner: refusal = %+v, want 422 %s", r, placement.ReasonRunnerRecordingOff)
	}
}
