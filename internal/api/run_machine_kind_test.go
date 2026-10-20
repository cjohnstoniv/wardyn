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
		ref := s.placementCapabilityRefusal(capsCtx, req, runPlacement{}, governanceCeiling{})
		if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Reason != string(placement.ReasonPlacementCapability) ||
			!strings.Contains(ref.body.Error, "Linux VM") {
			t.Errorf("%s: refusal = %+v, want 422 placement_capability naming Linux VM sandboxes", name, ref)
		}
	}
	if ref := s.placementCapabilityRefusal(capsCtx, createRunRequest{Placement: types.PlacementLocal, RunnerID: offers.String(), MachineKind: "linux_vm"}, runPlacement{}, governanceCeiling{}); ref != nil {
		t.Fatalf("a runner that offers Linux VMs was refused: %+v", ref)
	}
	// The VM's terminal is a plain shell: an interactive run that asks to open in the agent CLI is
	// refused up front, before it reaches the runner; a shell start, or a batch run, is not.
	agentStart := createRunRequest{Placement: types.PlacementLocal, RunnerID: offers.String(), MachineKind: "linux_vm", Interactive: true, InteractiveStart: "agent"}
	if ref := s.placementCapabilityRefusal(capsCtx, agentStart, runPlacement{}, governanceCeiling{}); ref == nil ||
		ref.body.Reason != string(placement.ReasonPlacementCapability) || !strings.Contains(ref.body.Error, "interactive_start") {
		t.Errorf("interactive_start agent on a VM: refusal = %+v, want 422 placement_capability naming interactive_start", ref)
	}
	shellStart := agentStart
	shellStart.InteractiveStart = "shell"
	if ref := s.placementCapabilityRefusal(capsCtx, shellStart, runPlacement{}, governanceCeiling{}); ref != nil {
		t.Errorf("interactive_start shell on a VM was refused: %+v", ref)
	}
	// A container run asks nothing of either runner, as before the knob existed.
	for _, id := range []uuid.UUID{offers, older} {
		for _, kind := range []string{"", "container"} {
			if ref := s.placementCapabilityRefusal(capsCtx, createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String(), MachineKind: kind}, runPlacement{}, governanceCeiling{}); ref != nil {
				t.Fatalf("a %q run was refused: %+v", kind, ref)
			}
		}
	}
	// A pooled run lands on any eligible runner, so every one of them must offer it.
	pooled := runPlacement{pooled: true, placement: types.PlacementLocal, eligible: []poolTarget{{runnerID: &offers}, {runnerID: &older}}}
	if ref := s.placementCapabilityRefusal(capsCtx, createRunRequest{MachineKind: "linux_vm"}, pooled, governanceCeiling{}); ref == nil {
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
