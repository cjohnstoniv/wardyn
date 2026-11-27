// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gpuRunner is the fake runner advertising the GPU kinds each class hands in.
type gpuRunner struct {
	*fakeRunner
	gpu map[types.ConfinementClass][]types.GPUKind
}

func (g *gpuRunner) Capabilities(ctx context.Context) (runner.Capabilities, error) {
	caps, err := g.fakeRunner.Capabilities(ctx)
	caps.GPU = g.gpu
	return caps, err
}

var gpuRestricted = map[string]map[string]bool{capFeature: {featureGPU: true}}

var nvidiaOnCC1 = map[types.ConfinementClass][]types.GPUKind{types.CC1: {types.GPUNvidia}}

func gpuAllow() types.CapabilityGrant {
	return grant(types.CapabilitySubjectAll, "", capFeature, featureGPU, types.CapabilityAllow)
}

// gpuStepCase is one stepGPU call: the request, the caller's grants and the executor.
type gpuStepCase struct {
	req      createRunRequest
	grants   []types.CapabilityGrant
	runner   runner.Runner // nil: no executor wired
	pooled   bool
	mode     foldMode
	enforced types.ConfinementClass
}

// runStepGPU runs stepGPU as a member under the restricted gpu value and returns what it wrote.
func runStepGPU(t *testing.T, tc gpuStepCase) (admitted bool, w *httptest.ResponseRecorder, cs *capStore) {
	t.Helper()
	cs = &capStore{grants: tc.grants, restricted: gpuRestricted}
	srv := capServer(cs)
	srv.cfg.Audit = &recRecorder{}
	srv.cfg.Now = time.Now
	srv.cfg.Runner = tc.runner
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(memberCtx([]string{"eng"}))
	w = httptest.NewRecorder()
	if tc.mode == 0 {
		tc.mode = foldCreate
	}
	if tc.enforced == "" {
		tc.enforced = types.CC1
	}
	f := &runFold{mode: tc.mode, w: w, r: r, req: &tc.req, enforced: tc.enforced, place: runPlacement{pooled: tc.pooled}}
	return srv.stepGPU(f, nil), w, cs
}

func wantGPURefusal(t *testing.T, admitted bool, w *httptest.ResponseRecorder, status int, reason, field string) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	if admitted || w.Code != status || body.Reason != reason || body.Field != field {
		t.Fatalf("admitted %v, %d %s; want refused %d reason %q field %q", admitted, w.Code, w.Body.String(), status, reason, field)
	}
	return body
}

func nvidiaRunner() runner.Runner { return &gpuRunner{fakeRunner: &fakeRunner{}, gpu: nvidiaOnCC1} }

func TestStepGPU_NoAskIsUntouched(t *testing.T) {
	admitted, w, cs := runStepGPU(t, gpuStepCase{req: createRunRequest{}})
	if !admitted || w.Body.Len() != 0 {
		t.Fatalf("no gpu asked: admitted %v body %q, want a quiet pass", admitted, w.Body.String())
	}
	if cs.fullTableReads != 0 || cs.groupDenyReads != 0 {
		t.Errorf("a run with no gpu read the capability grants (%d, %d)", cs.fullTableReads, cs.groupDenyReads)
	}
}

func TestStepGPU_UnknownKindIs400(t *testing.T) {
	admitted, w, _ := runStepGPU(t, gpuStepCase{req: createRunRequest{GPU: "cuda"}, grants: []types.CapabilityGrant{gpuAllow()}, runner: nvidiaRunner()})
	wantGPURefusal(t, admitted, w, http.StatusBadRequest, "gpu_unknown", "")
}

func TestStepGPU_NotGrantedIs403(t *testing.T) {
	deny := grant(types.CapabilitySubjectUser, capSub, capFeature, featureGPU, types.CapabilityDeny)
	for name, grants := range map[string][]types.CapabilityGrant{
		"no grant":                nil,
		"a deny and an allow":     {gpuAllow(), deny},
		"a * allow grants nobody": {grant(types.CapabilitySubjectAll, "", capFeature, capWildcard, types.CapabilityAllow)},
	} {
		t.Run(name, func(t *testing.T) {
			admitted, w, _ := runStepGPU(t, gpuStepCase{req: createRunRequest{GPU: "nvidia"}, grants: grants, runner: nvidiaRunner()})
			body := wantGPURefusal(t, admitted, w, http.StatusForbidden, "capability_feature", "")
			if body.Feature != featureGPU {
				t.Errorf("feature = %q, want %q", body.Feature, featureGPU)
			}
		})
	}
}

// The positive control of the offer check: with the grant and an offering executor the ask passes.
func TestStepGPU_GrantedAndOfferedPasses(t *testing.T) {
	admitted, w, _ := runStepGPU(t, gpuStepCase{req: createRunRequest{GPU: "nvidia"}, grants: []types.CapabilityGrant{gpuAllow()}, runner: nvidiaRunner()})
	if !admitted || w.Body.Len() != 0 {
		t.Fatalf("granted and offered: admitted %v, %d %s", admitted, w.Code, w.Body.String())
	}
	// The preview probes no runner, so it passes on the grant alone.
	admitted, _, _ = runStepGPU(t, gpuStepCase{req: createRunRequest{GPU: "nvidia"}, grants: []types.CapabilityGrant{gpuAllow()}, mode: foldPreview})
	if !admitted {
		t.Error("the preview refused a granted GPU ask")
	}
}

// TestStepGPU_GrantedRunKeepsItsGPU: through the whole create, a permitted GPU run's row records the kind and the
// dispatched sandbox spec carries it, and a run without the grant is refused before any row exists.
func TestStepGPU_GrantedRunKeepsItsGPU(t *testing.T) {
	const body = `"shape":"application","experience":"interactive",` + noRepos + `,"startup":{"kind":"command","command":"npm run dev"},"gpu":"nvidia"`
	cs := func(grants ...types.CapabilityGrant) *capStore {
		c := assignedStore(limitsProfile("open", types.GovernanceLimits{}))
		c.restricted, c.grants = gpuRestricted, grants
		return c
	}
	t.Run("refused without the grant, no row", func(t *testing.T) {
		srv, st, _ := govEscapeFixture(t, cs())
		srv.cfg.Runner = nvidiaRunner()
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-walled", []string{"eng"}, false), modeBody(body))
		if w.Code != http.StatusForbidden || errorReason(w) != "capability_feature" {
			t.Fatalf("create = %d %s, want 403 capability_feature", w.Code, w.Body.String())
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		if len(st.runs) != 0 {
			t.Errorf("a refused GPU run left %d rows", len(st.runs))
		}
	})
	t.Run("granted, the row and the spec carry it", func(t *testing.T) {
		srv, st, audit := govEscapeFixture(t, cs(gpuAllow()))
		gr := &gpuRunner{fakeRunner: &fakeRunner{}, gpu: map[types.ConfinementClass][]types.GPUKind{types.CC1: {types.GPUNvidia}, types.CC2: {types.GPUNvidia}, types.CC3: {types.GPUNvidia}}}
		srv.cfg.Runner = gr
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", govSession(t, "sub-walled", []string{"eng"}, false), modeBody(body))
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
		}
		st.mu.Lock()
		var run types.AgentRun
		for _, r := range st.runs {
			run = r
		}
		st.mu.Unlock()
		if run.GPU != types.GPUNvidia {
			t.Errorf("run row gpu = %q, want nvidia", run.GPU)
		}
		waitForRecAudit(t, audit, run.ID, "run.policy.resolve", "success")
		deadline := time.Now().Add(5 * time.Second)
		for {
			gr.mu.Lock()
			calls, spec := gr.createCalls, gr.lastSpec
			gr.mu.Unlock()
			if calls > 0 {
				if spec.Resources.GPU != types.GPUNvidia {
					t.Errorf("dispatched Resources.GPU = %q, want nvidia", spec.Resources.GPU)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the run was never dispatched")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func TestStepGPU_KindNotOfferedForClassIs422(t *testing.T) {
	grants := []types.CapabilityGrant{gpuAllow()}
	admitted, w, _ := runStepGPU(t, gpuStepCase{req: createRunRequest{GPU: "nvidia"}, grants: grants, runner: nvidiaRunner(), enforced: types.CC2})
	wantGPURefusal(t, admitted, w, http.StatusUnprocessableEntity, "placement_capability", "gpu")
	admitted, w, _ = runStepGPU(t, gpuStepCase{req: createRunRequest{GPU: "render"}, grants: grants, runner: nvidiaRunner()})
	wantGPURefusal(t, admitted, w, http.StatusUnprocessableEntity, "placement_capability", "gpu")
}

func TestStepGPU_VMIsRefused(t *testing.T) {
	admitted, w, _ := runStepGPU(t, gpuStepCase{req: createRunRequest{GPU: "nvidia", MachineKind: "linux_vm"}, grants: []types.CapabilityGrant{gpuAllow()}, runner: nvidiaRunner()})
	body := wantGPURefusal(t, admitted, w, http.StatusUnprocessableEntity, "placement_capability", "gpu")
	if body.Error != gpuNotAContainer {
		t.Errorf("error = %q, want the not-a-container sentence", body.Error)
	}
}

func TestStepGPU_LocalAndPooledAreRefused(t *testing.T) {
	grants := []types.CapabilityGrant{gpuAllow()}
	admitted, w, _ := runStepGPU(t, gpuStepCase{req: createRunRequest{GPU: "nvidia", Placement: types.PlacementLocal}, grants: grants, runner: nvidiaRunner()})
	wantGPURefusal(t, admitted, w, http.StatusUnprocessableEntity, "placement_capability", "gpu")
	admitted, w, _ = runStepGPU(t, gpuStepCase{req: createRunRequest{GPU: "nvidia"}, grants: grants, runner: nvidiaRunner(), pooled: true})
	wantGPURefusal(t, admitted, w, http.StatusUnprocessableEntity, "placement_capability", "gpu")
}

func TestStepGPU_KubernetesOnlyRefuses(t *testing.T) {
	k8s := &gpuRunner{fakeRunner: &fakeRunner{}} // no GPU map: no substrate offers one
	admitted, w, _ := runStepGPU(t, gpuStepCase{req: createRunRequest{GPU: "nvidia"}, grants: []types.CapabilityGrant{gpuAllow()}, runner: k8s})
	body := wantGPURefusal(t, admitted, w, http.StatusUnprocessableEntity, "placement_capability", "gpu")
	if !strings.Contains(body.Error, "offers no nvidia GPU") {
		t.Errorf("error = %q, want the not-offered sentence", body.Error)
	}
}

// The pooled and local refusals tell the person what to do next.
func TestStepGPU_NotHereGivesAWayForward(t *testing.T) {
	grants := []types.CapabilityGrant{gpuAllow()}
	for _, tc := range []gpuStepCase{
		{req: createRunRequest{GPU: "nvidia", Placement: types.PlacementLocal}, grants: grants, runner: nvidiaRunner()},
		{req: createRunRequest{GPU: "nvidia"}, grants: grants, runner: nvidiaRunner(), pooled: true},
	} {
		admitted, w, _ := runStepGPU(t, tc)
		body := wantGPURefusal(t, admitted, w, http.StatusUnprocessableEntity, "placement_capability", "gpu")
		if !strings.HasSuffix(body.Error, "Start the run without a GPU.") || body.Error != gpuNotHere {
			t.Errorf("error = %q, want the not-here sentence ending in a way forward", body.Error)
		}
		if strings.Contains(body.Error, "in this release") {
			t.Errorf("error %q promises a later release", body.Error)
		}
	}
}

func TestStepGPU_NoExecutorIs422(t *testing.T) {
	admitted, w, _ := runStepGPU(t, gpuStepCase{req: createRunRequest{GPU: "nvidia"}, grants: []types.CapabilityGrant{gpuAllow()}})
	body := wantGPURefusal(t, admitted, w, http.StatusUnprocessableEntity, "placement_capability", "gpu")
	if body.Error != gpuNotHere {
		t.Errorf("error = %q, want the not-here sentence", body.Error)
	}
}

func TestFeatureValuesListGPU(t *testing.T) {
	if got, err := canonicalGrantValue(capFeature, featureGPU); err != nil || got != featureGPU {
		t.Errorf("canonicalGrantValue(feature, gpu) = %q, %v; want gpu", got, err)
	}
}
