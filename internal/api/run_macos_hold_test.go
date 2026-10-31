// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A hold that autonomy level L1 or the policy's inner walls derive is refused at create and Review, in the
// words the placement-time twin uses, not by the guest launcher once the VM is up.
func TestStepMacOSHold_RefusesAHoldTheFoldDerives(t *testing.T) {
	background := func() createRunRequest {
		req := macReq(uuid.New())
		req.Agent, req.Task = "claude-code", "build it"
		return req
	}
	l1 := autonomySource{phrase: "the organisation's policy"}
	for _, tc := range []struct {
		name   string
		derive func(*createRunRequest)
		edit   func(*createRunRequest)
		kind   string // "" is a macOS VM
		want   bool   // refused
	}{
		{name: "autonomy L1 derives the hold", want: true,
			derive: func(req *createRunRequest) {
				s := capsServer(&fakeRunner{})
				if _, ok := s.autonomyDerive(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), req,
					types.AutonomyL1, "egress_open", l1, requestIsInteractive(*req)); !ok {
					t.Fatal("autonomyDerive refused a claude-code background run at L1")
				}
				if req.ToolApprovals != "hold" {
					t.Fatalf("autonomy L1 left tool_approvals = %q: the premise of this test is gone", req.ToolApprovals)
				}
			}},
		{name: "the inner walls derive the hold", want: true,
			derive: func(req *createRunRequest) {
				spec := types.RunPolicySpec{HarnessInnerWalls: &types.HarnessInnerWalls{Approvals: types.InnerApprovalsWardyn}}
				if applyInnerWallsHold(req, spec) == "" || req.ToolApprovals != "hold" {
					t.Fatalf("the inner walls left tool_approvals = %q: the premise of this test is gone", req.ToolApprovals)
				}
			}},
		{name: "autonomy L2 derives nothing", derive: func(req *createRunRequest) {
			s := capsServer(&fakeRunner{})
			s.autonomyDerive(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), req,
				types.AutonomyL2, "egress_open", l1, requestIsInteractive(*req))
		}},
		{name: "L1 on an interactive run derives nothing", edit: func(req *createRunRequest) { req.Interactive = true },
			derive: func(req *createRunRequest) {
				capsServer(&fakeRunner{}).autonomyDerive(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), req,
					types.AutonomyL1, "egress_open", l1, requestIsInteractive(*req))
			}},
		{name: "an exec run is not on the hold lane", edit: func(req *createRunRequest) { req.TaskMode, req.ToolApprovals = "exec", "hold" }},
		{name: "a container run keeps its hold lane", kind: "container", edit: func(req *createRunRequest) { req.ToolApprovals = "hold" }},
		{name: "a Linux VM run keeps its hold lane", kind: "linux_vm", edit: func(req *createRunRequest) { req.ToolApprovals = "hold" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := background()
			if tc.edit != nil {
				tc.edit(&req)
			}
			if tc.derive != nil {
				tc.derive(&req)
			}
			if tc.kind != "" {
				req.MachineKind = tc.kind
			}
			s := capsServer(&fakeRunner{})
			w := httptest.NewRecorder()
			f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req}
			ok := s.stepMacOSHold(f, nil)
			if !tc.want {
				if !ok {
					t.Fatalf("refused: %d %s", w.Code, w.Body)
				}
				return
			}
			body := w.Body.String()
			if ok || w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, `"placement_capability"`) ||
				!strings.Contains(body, macOSVMNoHold) || !strings.Contains(body, `"field":"tools"`) {
				t.Fatalf("ok = %v, %d %s; want 422 placement_capability on tools saying %q", ok, w.Code, body, macOSVMNoHold)
			}
		})
	}
}
