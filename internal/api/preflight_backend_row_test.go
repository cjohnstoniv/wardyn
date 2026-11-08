// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// foldedBackendRow runs Review's confinement step for req and then derives the setup rows the way Review does,
// for a run at class final, and returns the backend row.
func foldedBackendRow(t *testing.T, s *Server, req createRunRequest, place runPlacement, final types.ConfinementClass) SetupItem {
	t.Helper()
	f := &runFold{
		mode: foldPreflight, w: httptest.NewRecorder(),
		r:   httptest.NewRequest(http.MethodPost, "/api/v1/runs/preflight", nil).WithContext(capsCtx),
		req: &req, spec: types.RunPolicySpec{MinConfinementClass: types.CC1}, place: place,
	}
	if !s.stepConfinement(f, &foldRecorder{}) {
		t.Fatalf("stepConfinement refused: %s", f.w.(*httptest.ResponseRecorder).Body.String())
	}
	run := composer.RunInput{ConfinementClass: string(final)}
	items := s.deriveRunSetupItems(capsCtx, "", run, f.spec, secretsWith(), nil, nil, &f.placed)
	it, ok := findItem(items, "backend:"+string(final))
	if !ok {
		t.Fatalf("no backend:%s row in %+v", final, items)
	}
	return it
}

// Review's backend row reads the barriers of the runner the request pins, not the control plane's own runner
// (which here advertises all three).
func TestPreflightBackendRow_ReadsLocalRunnerClasses(t *testing.T) {
	s := capsServer(&fakeRunner{})
	id := installRunner(t, s, true, laptopCaps(types.CC1, types.CC2))
	req := createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String()}

	if it := foldedBackendRow(t, s, req, runPlacement{}, types.CC2); it.Status != "satisfied" {
		t.Errorf("CC2 row = %+v, want satisfied: the runner enforces it", it)
	}
	if it := foldedBackendRow(t, s, req, runPlacement{}, types.CC3); it.Status != "missing" || it.Fix == nil || it.Fix.Action != "none" {
		t.Errorf("CC3 row = %+v, want missing: the runner lacks it although the control plane's runner has it", it)
	}
}

// A pooled run's row reads its eligible candidates, with no executor asked at all.
func TestPreflightBackendRow_PoolUsesCandidates(t *testing.T) {
	s := capsServer(nil)
	place := runPlacement{
		pooled: true, row: types.RunnerPool{Scope: types.RunnerPoolOrganization},
		eligible: []poolTarget{{id: "a", facts: classes(types.CC1, types.CC2)}},
		targets:  []poolTarget{{id: "a", facts: classes(types.CC1, types.CC2)}},
	}
	req := createRunRequest{}
	if it := foldedBackendRow(t, s, req, place, types.CC2); it.Status != "satisfied" {
		t.Errorf("CC2 row = %+v, want satisfied: a candidate enforces it", it)
	}
	if it := foldedBackendRow(t, s, req, place, types.CC3); it.Status != "missing" {
		t.Errorf("CC3 row = %+v, want missing: no candidate enforces it", it)
	}
}

// A runner the fold could not read is not guessed from the control plane's runner: the row is unverified.
func TestPreflightBackendRow_RunnerUnreadIsCheckedAtRunner(t *testing.T) {
	s := capsServer(&fakeRunner{})
	id := installRunner(t, s, false, laptopCaps(types.CC1, types.CC2, types.CC3))
	req := createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String()}
	it := foldedBackendRow(t, s, req, runPlacement{}, types.CC3)
	if it.Status != "unverified" || it.Detail != "Checked at the runner when the run starts." {
		t.Errorf("row = %+v, want unverified, checked at the runner", it)
	}
}

// The compose panel has no placement: its backend row still probes the control plane's own runner.
func TestComposeBackendRow_Unchanged(t *testing.T) {
	srv := newSetupTestServer()
	srv.cfg.Runner = setupTestRunner{caps: runner.Capabilities{
		ConfinementClasses: []types.ConfinementClass{types.CC1, types.CC2},
		Resolved:           map[types.ConfinementClass]string{types.CC2: "oci/runsc"},
	}}
	for _, tc := range []struct {
		class  types.ConfinementClass
		status string
	}{{types.CC2, "satisfied"}, {types.CC3, "missing"}} {
		run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral", ConfinementClass: string(tc.class)}
		items := srv.deriveSetupItems(context.Background(), "", run, types.RunPolicySpec{}, secretsWith(), nil)
		got, ok := findItem(items, "backend:"+string(tc.class))
		if !ok || got.Status != tc.status {
			t.Fatalf("%s row = %+v (ok=%v), want %s", tc.class, got, ok, tc.status)
		}
		if tc.status == "satisfied" && !strings.Contains(got.Detail, "oci/runsc") {
			t.Errorf("%s detail = %q, want the control plane's substrate named", tc.class, got.Detail)
		}
	}
}
