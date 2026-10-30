// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

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

// The create path asks a Mac for a share drive: refused with MAC-D13 on a runner whose caps lack Drives,
// admitted on one that carries them.
func TestStepPlacementCapability_MacOSVMShareDrive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		drives bool // the Mac's runner carries drives
		want   string
	}{
		{name: "a runner that predates drives", want: "This runner's version can't attach drives to a macOS VM. Update the runner, or run without drives."},
		{name: "a runner that carries drives", drives: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := capsServer(&fakeRunner{})
			mc := readyMac()
			mc.Drives = tc.drives
			req := macReq(macRunner(t, s, mc, roomyRunner))
			req.Agent, req.Task = "claude-code", "build it"
			w := httptest.NewRecorder()
			f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req,
				drives: []runDrive{{resolved: types.ResolvedDrive{Drive: types.UserDrive{Name: "Team", Backend: types.DriveBackendHostPath}}}}}
			ok := s.stepPlacementCapability(f, nil)
			if tc.want == "" {
				if !ok {
					t.Fatalf("refused: %d %s", w.Code, w.Body)
				}
				return
			}
			body := w.Body.String()
			if ok || w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, `"placement_capability"`) || !strings.Contains(body, tc.want) ||
				!strings.Contains(body, `"field":"drives"`) {
				t.Fatalf("ok = %v, %d %s; want 422 placement_capability on drives saying %q", ok, w.Code, body, tc.want)
			}
		})
	}
}

// A run's drives are sorted for a macOS VM: an admin or team drive in a host folder is a shared folder,
// refused only on a runner that predates them (MAC-D13); a Docker or cluster volume is never opened; a
// person's drive and the kept work folder are refused as before (MAC-F01) until a disk carries them.
func TestPlacementCapability_MacOSVMDrives(t *testing.T) {
	const (
		d13     = "This runner's version can't attach drives to a macOS VM. Update the runner, or run without drives."
		f01     = "A macOS VM sandbox has no drives in this release."
		volume  = "Team is a Docker volume, which a macOS VM can't open."
		cluster = "Team is a cluster volume, which a macOS VM can't open."
	)
	// driveAsk is the drive fields of the ask; the ask itself holds slices and cannot be compared.
	type driveAsk struct {
		ShareDrives, Drives, ClusterVolume bool
		VolumeDrive                        string
	}
	secondRunner := uuid.New()
	drive := func(edit func(*types.UserDrive)) runDrive {
		d := types.UserDrive{Name: "Team", Backend: types.DriveBackendHostPath}
		edit(&d)
		return runDrive{resolved: types.ResolvedDrive{Drive: d}}
	}
	share := drive(func(*types.UserDrive) {})
	team := drive(func(d *types.UserDrive) { d.Team = true })
	secondPlace := drive(func(d *types.UserDrive) {
		d.Backend, d.AltBackend, d.AltHostRoot, d.AltRunnerID = types.DriveBackendK8sPVCStatic, types.DriveBackendHostPath, "/Volumes/team", &secondRunner
	})
	teamVolume := drive(func(d *types.UserDrive) { d.Backend, d.Team = types.DriveBackendDockerVolume, true })
	clusterDrive := drive(func(d *types.UserDrive) { d.Backend = types.DriveBackendK8sPVCStatic })
	personal := drive(func(d *types.UserDrive) { d.Backend, d.OwnerSubject = types.DriveBackendDockerVolume, "alice" })
	managed := drive(func(d *types.UserDrive) { d.Backend = types.DriveBackendDockerVolume })
	for _, tc := range []struct {
		name    string
		drives  []runDrive
		keep    *keptWorkFolder
		caps    bool // the Mac's runner carries drives
		want    string
		placed  bool
		wantAsk driveAsk
	}{
		{name: "a share drive", drives: []runDrive{share}, caps: true, placed: true, wantAsk: driveAsk{ShareDrives: true}},
		{name: "a share drive on a runner that predates them", drives: []runDrive{share}, want: d13, wantAsk: driveAsk{ShareDrives: true}},
		{name: "a team drive", drives: []runDrive{team}, caps: true, placed: true, wantAsk: driveAsk{ShareDrives: true}},
		{name: "a drive on its second place", drives: []runDrive{secondPlace}, caps: true, placed: true, wantAsk: driveAsk{ShareDrives: true}},
		{name: "a team volume", drives: []runDrive{teamVolume}, caps: true, want: volume, wantAsk: driveAsk{VolumeDrive: "Team"}},
		{name: "a cluster volume", drives: []runDrive{clusterDrive}, caps: true, want: cluster, wantAsk: driveAsk{VolumeDrive: "Team", ClusterVolume: true}},
		{name: "a person's drive", drives: []runDrive{personal}, caps: true, want: f01, wantAsk: driveAsk{Drives: true}},
		{name: "a managed drive", drives: []runDrive{managed}, caps: true, want: f01, wantAsk: driveAsk{Drives: true}},
		{name: "a kept work folder", keep: &keptWorkFolder{}, caps: true, want: f01, wantAsk: driveAsk{Drives: true}},
		{name: "a share beside a person's drive", drives: []runDrive{share, personal}, caps: true, want: f01, wantAsk: driveAsk{ShareDrives: true, Drives: true}},
		{name: "a share beside a volume", drives: []runDrive{share, teamVolume}, caps: true, want: volume,
			wantAsk: driveAsk{ShareDrives: true, VolumeDrive: "Team"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ask macOSVMAsk
			ask.askDrives(tc.drives, tc.keep)
			if got := (driveAsk{ask.ShareDrives, ask.Drives, ask.ClusterVolume, ask.VolumeDrive}); got != tc.wantAsk {
				t.Fatalf("ask = %+v, want %+v", got, tc.wantAsk)
			}
			s := capsServer(&fakeRunner{})
			mc := readyMac()
			mc.Drives = tc.caps
			req := macReq(macRunner(t, s, mc, roomyRunner))
			ref := s.placementCapabilityRefusalFor(capsCtx, req, runPlacement{}, governanceCeiling{}, 0, ask)
			// Each family gets its own sentence: a refusal that is neither volume's names neither.
			if ref != nil && tc.want != volume && tc.want != cluster {
				for _, sentence := range []string{volume, cluster} {
					if strings.Contains(ref.body.Error, sentence) {
						t.Fatalf("refusal %q names a volume sentence it should not: %q", ref.body.Error, sentence)
					}
				}
			}
			if tc.placed {
				if ref != nil {
					t.Fatalf("refused: %+v", ref.body)
				}
				return
			}
			if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Error != tc.want || ref.body.Field != "drives" {
				t.Fatalf("refusal = %+v, want 422 on drives saying %q", ref, tc.want)
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
