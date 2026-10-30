// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Each thing a macOS VM does not do in this release is refused at create and Review in the launcher's
// words, naming the request field, before a Mac is asked to start the VM (MAC-F01, MAC-A05, MAC-A06).
func TestStepPlacementCapability_MacOSVMRefusesWhatItCannotDo(t *testing.T) {
	task := func(req *createRunRequest) { req.Agent, req.Task = "claude-code", "build it" }
	askFirst := types.ToolRule{Tool: "Bash", Effect: types.ToolHold}
	for _, tc := range []struct {
		name  string
		edit  func(*createRunRequest)
		fold  func(*runFold)
		want  string // "" means the run is placed
		field string
	}{
		{name: "a plain background task", edit: task},
		{name: "a drive", edit: task, fold: func(f *runFold) { f.drives = []runDrive{{}} },
			want: "A macOS VM sandbox has no drives in this release.", field: "drives"},
		{name: "a kept work folder", edit: task, fold: func(f *runFold) { f.keep = &keptWorkFolder{} },
			want: "A macOS VM sandbox has no drives in this release.", field: "drives"},
		{name: "a second harness with its own prompt", edit: task,
			fold: func(f *runFold) {
				f.harnesses = []types.RunHarness{{Harness: "claude-code", Starts: true}, {Harness: "codex-cli", Task: "review it"}}
			},
			want: "A macOS VM starts one agent per run in this release, and another harness in this run has its own starting prompt. Remove that prompt, or choose a container or a Linux VM.", field: "tools"},
		{name: "a second harness with no prompt", edit: task,
			fold: func(f *runFold) {
				f.harnesses = []types.RunHarness{{Harness: "claude-code", Starts: true}, {Harness: "none"}}
			}},
		{name: "an Ask first rule in the policy", edit: task, fold: func(f *runFold) { f.spec.ToolRules = []types.ToolRule{askFirst} },
			want: "A macOS VM can't hold a tool call for approval in this release, and this run's tool rules or tool approvals would. Change them, or choose a container or a Linux VM.", field: "tools"},
		{name: "an Ask first rule of the person's", edit: func(req *createRunRequest) {
			task(req)
			req.Tools = []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{askFirst}}}
		}, want: "tool rules or tool approvals would", field: "tools"},
		{name: "an Ask first default of the person's", edit: func(req *createRunRequest) {
			task(req)
			req.Tools = []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness, DefaultEffect: types.ToolHold}}
		}, want: "tool rules or tool approvals would", field: "tools"},
		{name: "tool approvals on hold", edit: func(req *createRunRequest) { task(req); req.ToolApprovals = "hold" },
			want: "A macOS VM can't hold a tool call for approval in this release, and this run's tool rules or tool approvals would. Change them, or choose a container or a Linux VM.", field: "tools"},
		// Run it is a rule of the person's too, and holds like any other: there is no rule to change to.
		{name: "a Run it rule of the person's", edit: func(req *createRunRequest) {
			task(req)
			req.Tools = []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolAllow}}}}
		}, want: "tool rules or tool approvals would", field: "tools"},
		{name: "a Block it rule puts the run on the hold lane too", edit: task,
			fold: func(f *runFold) { f.spec.ToolRules = []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}} },
			want: "tool rules or tool approvals would", field: "tools"},
		// The same rules where nothing holds: an interactive run, an exec run, an agent with no hold lane.
		{name: "Ask first on an interactive run", edit: func(req *createRunRequest) { req.Agent = "claude-code" },
			fold: func(f *runFold) { f.spec.ToolRules = []types.ToolRule{askFirst} }},
		{name: "Ask first on an exec run", edit: func(req *createRunRequest) { task(req); req.TaskMode = "exec" },
			fold: func(f *runFold) { f.spec.ToolRules = []types.ToolRule{askFirst} }},
		{name: "Ask first on an agent with no hold lane", edit: func(req *createRunRequest) { req.Agent, req.Task = "codex-cli", "build it" },
			fold: func(f *runFold) { f.spec.ToolRules = []types.ToolRule{askFirst} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := capsServer(&fakeRunner{})
			id := macRunner(t, s, func() *runnerwire.MacVMCaps {
				mc := readyMac()
				mc.Harnesses = append(mc.Harnesses, "codex-cli")
				return mc
			}(), roomyRunner)
			req := macReq(id)
			tc.edit(&req)
			w := httptest.NewRecorder()
			f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req}
			if tc.fold != nil {
				tc.fold(f)
			}
			ok := s.stepPlacementCapability(f, nil)
			if tc.want == "" {
				if !ok {
					t.Fatalf("refused: %d %s", w.Code, w.Body)
				}
				return
			}
			body := w.Body.String()
			if ok || w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, `"placement_capability"`) || !strings.Contains(body, tc.want) ||
				!strings.Contains(body, `"field":"`+tc.field+`"`) {
				t.Fatalf("ok = %v, %d %s; want 422 placement_capability on %s saying %q", ok, w.Code, body, tc.field, tc.want)
			}
		})
	}
}

// The same shape on a container, and on a Linux VM, is not this refusal's business: only a macOS VM
// is asked.
func TestStepPlacementCapability_MacOSVMRefusalsLeaveOtherKindsAlone(t *testing.T) {
	s := capsServer(&fakeRunner{})
	id := macRunner(t, s, readyMac(), roomyRunner)
	req := createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String(), Agent: "claude-code", Task: "build it", ToolApprovals: "hold"}
	f := &runFold{w: httptest.NewRecorder(), r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req,
		drives: []runDrive{{}}, harnesses: []types.RunHarness{{Harness: "claude-code"}, {Harness: "codex-cli", Task: "x"}}}
	if !s.stepPlacementCapability(f, nil) {
		t.Fatalf("a container run was refused with the macOS VM's sentences: %s", f.w.(*httptest.ResponseRecorder).Body)
	}
}

// A service on a macOS VM was already refused at create (the twin of the launcher's WARDYN_SERVICE
// sentence); pinned here with the other two so the three cannot drift apart.
func TestServiceRefusal_MacOSVM(t *testing.T) {
	ref := serviceRefusal(createRunRequest{MachineKind: "macos_vm", Service: &types.RunService{}})
	if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Error != "service: a service runs in a container sandbox, not a virtual machine." {
		t.Fatalf("refusal = %+v", ref)
	}
}
