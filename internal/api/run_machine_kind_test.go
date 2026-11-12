// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A Linux VM is placed only on a registered runner whose caps offer it: the deployment's own
// executor, an older runner and a runner whose operator left VMs off all refuse, before anything
// is created.
func TestPlacementCapability_LinuxVMNeedsARunnerThatOffersIt(t *testing.T) {
	s := capsServer(&fakeRunner{})
	vmCaps := laptopCaps(types.CC1)
	vmCaps.LinuxVM = &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13"}
	offers := installRunner(t, s, true, vmCaps)
	older := installRunner(t, s, true, laptopCaps(types.CC1))

	for name, req := range map[string]createRunRequest{
		"remote":           {Placement: types.PlacementRemote, MachineKind: "linux_vm"},
		"unplaced":         {MachineKind: "linux_vm"},
		"no linux_vm caps": {Placement: types.PlacementLocal, RunnerID: older.String(), MachineKind: "linux_vm"},
	} {
		ref := s.placementCapabilityRefusal(capsCtx, req, runPlacement{}, governanceCeiling{}, 0)
		if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Reason != string(placement.ReasonPlacementCapability) ||
			!strings.Contains(ref.body.Error, "Linux VM") {
			t.Errorf("%s: refusal = %+v, want 422 placement_capability naming Linux VM sandboxes", name, ref)
		}
	}
	if ref := s.placementCapabilityRefusal(capsCtx, createRunRequest{Placement: types.PlacementLocal, RunnerID: offers.String(), MachineKind: "linux_vm"}, runPlacement{}, governanceCeiling{}, 0); ref != nil {
		t.Fatalf("a runner that offers Linux VMs was refused: %+v", ref)
	}
	// The VM's terminal is a plain shell: an interactive run that asks to open in the agent CLI is
	// refused up front, before it reaches the runner; a shell start, or a batch run, is not.
	agentStart := createRunRequest{Placement: types.PlacementLocal, RunnerID: offers.String(), MachineKind: "linux_vm", Interactive: true, InteractiveStart: "agent"}
	if ref := s.placementCapabilityRefusal(capsCtx, agentStart, runPlacement{}, governanceCeiling{}, 0); ref == nil ||
		ref.body.Reason != string(placement.ReasonPlacementCapability) || ref.body.Field != "interactive_start" || !strings.Contains(ref.body.Error, "interactive_start") {
		t.Errorf("interactive_start agent on a VM: refusal = %+v, want 422 placement_capability naming interactive_start", ref)
	}
	shellStart := agentStart
	shellStart.InteractiveStart = "shell"
	if ref := s.placementCapabilityRefusal(capsCtx, shellStart, runPlacement{}, governanceCeiling{}, 0); ref != nil {
		t.Errorf("interactive_start shell on a VM was refused: %+v", ref)
	}
	// A container run asks nothing of either runner, as before the knob existed.
	for _, id := range []uuid.UUID{offers, older} {
		for _, kind := range []string{"", "container"} {
			if ref := s.placementCapabilityRefusal(capsCtx, createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String(), MachineKind: kind}, runPlacement{}, governanceCeiling{}, 0); ref != nil {
				t.Fatalf("a %q run was refused: %+v", kind, ref)
			}
		}
	}
	// A pooled run lands on any eligible runner, so every one of them must offer it.
	pooled := runPlacement{pooled: true, placement: types.PlacementLocal, eligible: []poolTarget{{runnerID: &offers}, {runnerID: &older}}}
	if ref := s.placementCapabilityRefusal(capsCtx, createRunRequest{MachineKind: "linux_vm"}, pooled, governanceCeiling{}, 0); ref == nil {
		t.Fatal("a pool with a runner that does not offer Linux VMs placed one")
	}
}

func TestDecodeAndValidateCreateRun_UnknownMachineKind(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(`{"agent":"claude-code","machine_kind":"windows_vm"}`))
	if _, _, _, _, ok := h.srv.decodeAndValidateCreateRun(rec, req); ok || rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), reasonMachineKindUnknown) {
		t.Fatalf("status %d body %s, want 400 %s", rec.Code, rec.Body.String(), reasonMachineKindUnknown)
	}
}

type vmMachineStore struct {
	ownedRunners
	got map[uuid.UUID]types.VMMachine
}

func (v vmMachineStore) SetRunVMMachine(_ context.Context, id uuid.UUID, m types.VMMachine) error {
	v.got[id] = m
	return nil
}

// A VM run records the machine its runner started, and one whose machine is missing never runs; a
// container run writes nothing.
func TestRecordVMMachine(t *testing.T) {
	st := vmMachineStore{ownedRunners: ownedRunners{rows: map[uuid.UUID]types.Runner{}}, got: map[uuid.UUID]types.VMMachine{}}
	s := &Server{cfg: Config{Store: st}}
	m := &types.VMMachine{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", Accelerator: "kvm", QEMUVersion: "10.0.13"}
	ctr := types.AgentRun{ID: uuid.New()}
	if err := s.recordVMMachine(context.Background(), ctr, runner.Sandbox{Machine: m}); err != nil || len(st.got) != 0 {
		t.Fatalf("container run: %v, wrote %v", err, st.got)
	}
	vm := types.AgentRun{ID: uuid.New(), MachineKind: types.MachineLinuxVM}
	if err := s.recordVMMachine(context.Background(), vm, runner.Sandbox{Machine: &types.VMMachine{MachineType: "pc-q35-10.0"}}); err == nil {
		t.Fatal("a VM with an incomplete machine was accepted")
	}
	if err := s.recordVMMachine(context.Background(), vm, runner.Sandbox{Machine: m}); err != nil || st.got[vm.ID] != *m {
		t.Fatalf("VM run: %v, wrote %v", err, st.got)
	}
	plain := &Server{cfg: Config{Store: ownedRunners{rows: map[uuid.UUID]types.Runner{}}}}
	if err := plain.recordVMMachine(context.Background(), vm, runner.Sandbox{Machine: m}); err == nil {
		t.Fatal("a store that cannot record the machine let the VM run")
	}
}

// A Linux VM run that carries a folder goes only to a runner that shares one into the guest: one
// that offers VMs without virtiofs (or from before the flag) is refused before create, with its
// sentence, and the same run without a folder, or as a container, is not.
func TestPlacementCapability_LinuxVMWithAFolderNeedsVirtiofs(t *testing.T) {
	s := capsServer(&fakeRunner{})
	shares, noShares := laptopCaps(types.CC1), laptopCaps(types.CC1)
	shares.LinuxVM = &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Virtiofs: true}
	noShares.LinuxVM = &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13"}
	with, without := installRunner(t, s, true, shares), installRunner(t, s, true, noShares)
	vm := func(id uuid.UUID) createRunRequest {
		return createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String(), MachineKind: "linux_vm"}
	}
	ref := s.placementCapabilityRefusal(capsCtx, vm(without), runPlacement{}, governanceCeiling{}, 1)
	if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Reason != string(placement.ReasonPlacementCapability) ||
		!strings.Contains(ref.body.Error, "cannot share a folder into a Linux VM sandbox") {
		t.Fatalf("a VM with a folder on a runner without virtiofs: refusal = %+v", ref)
	}
	if ref := s.placementCapabilityRefusal(capsCtx, vm(with), runPlacement{}, governanceCeiling{}, 1); ref != nil {
		t.Fatalf("a runner that shares folders into VMs was refused: %+v", ref)
	}
	if ref := s.placementCapabilityRefusal(capsCtx, vm(without), runPlacement{}, governanceCeiling{}, 0); ref != nil {
		t.Fatalf("a VM with no folder was refused: %+v", ref)
	}
	container := vm(without)
	container.MachineKind = ""
	if ref := s.placementCapabilityRefusal(capsCtx, container, runPlacement{}, governanceCeiling{}, 1); ref != nil {
		t.Fatalf("a container with a folder was refused: %+v", ref)
	}
}

// A Linux VM run that carries a drive goes only to a runner that says it shares one into the guest:
// an older or Kubernetes runner is refused at Review with the sentence that says what to do, on the
// drives field. The same run without a drive, or as a container, or on a runner that says so is not.
func TestStepPlacementCapability_LinuxVMWithADriveNeedsTheCap(t *testing.T) {
	s := capsServer(&fakeRunner{})
	with, without := laptopCaps(types.CC1), laptopCaps(types.CC1)
	with.LinuxVM = &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Virtiofs: true, Drives: true}
	without.LinuxVM = &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Virtiofs: true}
	offers, older := installRunner(t, s, true, with), installRunner(t, s, true, without)
	team := []runDrive{{resolved: types.ResolvedDrive{Drive: types.UserDrive{Name: "Team", Backend: types.DriveBackendHostPath}}}}

	for _, tc := range []struct {
		name   string
		runner uuid.UUID
		kind   string
		drives []runDrive
		want   bool
	}{
		{name: "a drive on a runner that lacks the cap", runner: older, kind: "linux_vm", drives: team, want: true},
		{name: "a drive on a runner that has it", runner: offers, kind: "linux_vm", drives: team},
		{name: "no drive on a runner that lacks it", runner: older, kind: "linux_vm"},
		{name: "a container with a drive", runner: older, drives: team},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := createRunRequest{Placement: types.PlacementLocal, RunnerID: tc.runner.String(), MachineKind: tc.kind}
			w := httptest.NewRecorder()
			f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req, drives: tc.drives}
			ok := s.stepPlacementCapability(f, nil)
			if ok == tc.want {
				t.Fatalf("ok = %v, %d %s", ok, w.Code, w.Body.String())
			}
			if tc.want && (w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"placement_capability"`) ||
				!strings.Contains(w.Body.String(), `"drives"`) || !strings.Contains(w.Body.String(), "Update the runner, or run without drives.")) {
				t.Fatalf("%d %s, want 422 placement_capability on drives saying to update the runner", w.Code, w.Body.String())
			}
		})
	}
}

// A kept work folder on a Linux VM is refused at Review in its own words, on the keep_work_folder field,
// whatever the runner reports: it is a Docker or cluster volume, which a Linux VM never opens, so a runner
// that shares drives must not pass it only for the start to refuse it, and one that does not must not be
// told about drives the run never named. A container keeping its folder, and a VM with a drive and no
// kept folder, are not refused by it.
func TestStepPlacementCapability_LinuxVMKeptFolderIsRefusedAtReview(t *testing.T) {
	s := capsServer(&fakeRunner{})
	with, without := laptopCaps(types.CC1), laptopCaps(types.CC1)
	with.LinuxVM = &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Virtiofs: true, Drives: true}
	without.LinuxVM = &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Virtiofs: true}
	offers, older := installRunner(t, s, true, with), installRunner(t, s, true, without)
	keptFolder := &keptWorkFolder{row: types.UserDrive{Backend: types.DriveBackendDockerVolume}}
	keptFolderTeam := []runDrive{{resolved: types.ResolvedDrive{Drive: types.UserDrive{Name: "Team", Backend: types.DriveBackendHostPath}}}}

	for _, tc := range []struct {
		name   string
		runner uuid.UUID
		kind   string
		keep   *keptWorkFolder
		drives []runDrive
		want   bool
	}{
		{name: "a kept folder on a runner that lacks the drive cap", runner: older, kind: "linux_vm", keep: keptFolder, want: true},
		{name: "a kept folder on a runner that has it", runner: offers, kind: "linux_vm", keep: keptFolder, want: true},
		{name: "a kept folder beside a team drive", runner: offers, kind: "linux_vm", keep: keptFolder, drives: keptFolderTeam, want: true},
		{name: "a team drive and no kept folder", runner: offers, kind: "linux_vm", drives: keptFolderTeam},
		{name: "a container keeping its folder", runner: older, keep: keptFolder},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := createRunRequest{Placement: types.PlacementLocal, RunnerID: tc.runner.String(), MachineKind: tc.kind, KeepWorkFolder: tc.keep != nil}
			w := httptest.NewRecorder()
			f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req, keep: tc.keep, drives: tc.drives}
			ok := s.stepPlacementCapability(f, nil)
			if ok == tc.want {
				t.Fatalf("ok = %v, %d %s", ok, w.Code, w.Body.String())
			}
			if !tc.want {
				return
			}
			body := w.Body.String()
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, `"placement_capability"`) || !strings.Contains(body, `"keep_work_folder"`) ||
				!strings.Contains(body, "can't keep its work folder as a drive") || strings.Contains(body, "Update the runner") {
				t.Fatalf("%d %s, want 422 placement_capability on keep_work_folder naming the kept folder", w.Code, body)
			}
		})
	}
}

// The create fold counts the folders the sandbox would get from the resolved policy, so a VM run
// whose policy mounts a folder is refused at create on a runner without virtiofs.
func TestStepPlacementCapability_CountsThePolicysFolders(t *testing.T) {
	s := capsServer(&fakeRunner{})
	caps := laptopCaps(types.CC1)
	caps.LinuxVM = &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13"}
	id := installRunner(t, s, true, caps)
	for _, mounts := range [][]types.WorkspaceMount{nil, {{Source: "/home/me/project", Target: "/home/agent/work"}}} {
		req := createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String(), MachineKind: "linux_vm"}
		w := httptest.NewRecorder()
		f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req,
			spec: types.RunPolicySpec{WorkspaceMounts: mounts}}
		ok := s.stepPlacementCapability(f, nil)
		if ok != (mounts == nil) || (!ok && !strings.Contains(w.Body.String(), "cannot share a folder into a Linux VM sandbox")) {
			t.Fatalf("%d folders: step ok = %v, body %s", len(mounts), ok, w.Body.String())
		}
	}
}
