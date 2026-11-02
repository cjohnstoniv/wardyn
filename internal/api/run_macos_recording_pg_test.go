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

// TestPG_MacOSVMRecordingRequiredRefusesInItsOwnWords: where the organisation requires recording, a
// macOS VM run is refused at the pool with the macOS sentence (a macOS VM is not recorded in this
// release), not as a pool whose runners do not record; a container in the same pool still places, and
// nothing is refused where recording is only preferred.
func TestPG_MacOSVMRecordingRequiredRefusesInItsOwnWords(t *testing.T) {
	e := newPlacementEnv(t)
	e.srv.cfg.RecordingStore = fsRecordingStore(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	id := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.setRecords(id, true)
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
	isRecording := func(r *runRefusal) bool {
		return r != nil && strings.Contains(r.body.Error, "isn't recorded in this release")
	}

	e.requireRecording(types.RecordingPreferred)
	if r := resolve("macos_vm"); isRecording(r) {
		t.Fatalf("preferred: refused for recording: %+v", r.body)
	}
	e.requireRecording(types.RecordingRequired)
	r := resolve("macos_vm")
	if r == nil || r.status != http.StatusUnprocessableEntity || r.body.Reason != string(placement.ReasonPlacementCapability) ||
		r.body.Field != "machine_kind" || r.body.Error != "Recording is required"+macOSNoRecording {
		t.Fatalf("required, a macOS VM: refusal = %+v, want 422 placement_capability on machine_kind with the required sentence", r)
	}
	if r := resolve(""); isRecording(r) {
		t.Fatalf("required, a container: refused with the macOS sentence: %+v", r.body)
	}
}
