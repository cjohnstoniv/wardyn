// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
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

// machineKindJudgement is a pooled local create-time judgement of a request of kind, with the organisation's
// machine-kind denials and the given classes among the pool's runners.
func machineKindJudgement(t *testing.T, kind string, denials []types.MachineKindDenial, trusts ...types.RunnerTrustClass) (localJudgement, *runRefusal) {
	t.Helper()
	f := newComponentFixture(t)
	f.srv.cfg.Store = localPlanTestStore{f.st}
	f.st.siteConfig = types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true, MachineKindDenials: denials}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	return f.srv.localPlacementRefusal(r, createRunRequest{Placement: placement.Local, MachineKind: kind}, true, trusts, nil, types.RunPolicySpec{}, governanceCeiling{}, runComponents{}, nil, nil, composer.Baseline{})
}

// P4: a super admin denies Linux VMs on personal runners and leaves them on managed_device ones. A pool of both
// places the run on the managed class; a pool of personal runners only is refused with the sentence that says why.
func TestPlacementTrust_DenyOnOneClassAllowOnAnother(t *testing.T) {
	deny := []types.MachineKindDenial{{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM}}
	j, ref := machineKindJudgement(t, "linux_vm", deny, types.RunnerTrustPersonal, types.RunnerTrustManagedDevice)
	if ref != nil || j.trust != types.RunnerTrustManagedDevice {
		t.Fatalf("personal and managed_device, linux_vm denied on personal: %+v trust %q, want admitted at managed_device", ref, j.trust)
	}
	_, ref = machineKindJudgement(t, "linux_vm", deny, types.RunnerTrustPersonal)
	const sentence = "Your organisation does not allow Linux VM sandboxes on personal runners."
	if ref == nil || ref.status != http.StatusForbidden || ref.body.Reason != string(placement.ReasonPlacementDenied) || ref.body.Error != sentence {
		t.Fatalf("personal only: %+v, want 403 placement_denied saying %q", ref, sentence)
	}
	// Another kind, and no list at all, are untouched.
	if j, ref := machineKindJudgement(t, "", deny, types.RunnerTrustPersonal); ref != nil || j.trust != types.RunnerTrustPersonal {
		t.Errorf("a container on personal with linux_vm denied there: %+v trust %q", ref, j.trust)
	}
	if j, ref := machineKindJudgement(t, "linux_vm", nil, types.RunnerTrustPersonal); ref != nil || j.trust != types.RunnerTrustPersonal {
		t.Errorf("a linux_vm on personal with no list: %+v trust %q", ref, j.trust)
	}
}

// P5 and the order (a personal Mac is told why by the managed-Mac gate): the create's eligibility step leaves a
// macOS VM to the capability step whatever the list says, and that step refuses a personal Mac with v0.10.2's
// reason and sentences, to a member and to a security operator.
func TestPlacementTrust_PersonalMacRefusedAMacOSVM(t *testing.T) {
	member := withOIDCRole(capsCtx, oidc.RoleUser)
	operator := withOIDCRole(capsCtx, oidc.RoleSecurityAdmin)
	for _, denials := range [][]types.MachineKindDenial{nil, {{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM}}} {
		if _, ref := machineKindJudgement(t, "macos_vm", denials, types.RunnerTrustPersonal); ref != nil {
			t.Fatalf("the eligibility step refused a personal Mac with denials %v: %+v, want it left to macOSHostGate", denials, ref)
		}
		for name, tc := range map[string]struct {
			ctx  context.Context
			want string
		}{
			"member":   {member, "Mac mini is not marked as company-managed. Ask a security admin to mark it as a managed device."},
			"operator": {operator, "Mac mini is not marked as company-managed. Mark it with `wardyn runner trust "},
		} {
			s := capsServer(&fakeRunner{})
			id := macRunner(t, s, readyMac(), func(r *types.Runner) { r.TrustClass = types.RunnerTrustPersonal })
			ref := s.placementCapabilityRefusalFor(tc.ctx, macReq(id), runPlacement{}, governanceCeiling{}, 0, macOSVMAsk{})
			if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Reason != string(placement.ReasonPlacementCapability) || !strings.Contains(ref.body.Error, tc.want) {
				t.Errorf("%s, denials %v: refusal = %+v, want 422 placement_capability saying %q", name, denials, ref, tc.want)
			}
		}
	}
}

// A runner of the dispatch tests' run: the stored row classification reads for the runner's trust class and owner kind.
type localDispatchRunnerStore struct {
	localDispatchStore
	row types.Runner
}

func (s localDispatchRunnerStore) GetRunner(context.Context, uuid.UUID) (types.Runner, error) {
	return s.row, nil
}

// dispatchOfKind classifies a local run of kind bound to a runner of class, against site configuration sc, and
// answers whether dispatch went on, with the audit and the run.
func dispatchOfKind(t *testing.T, kind types.MachineKind, class types.RunnerTrustClass, sc types.SiteConfig) (bool, *recRecorder, localDispatchRunnerStore, types.AgentRun) {
	t.Helper()
	srv, ls, audit, run := localDispatchFixture(t, types.PlacementLocal)
	id := uuid.New()
	run.RunnerID, run.MachineKind = &id, kind
	ls.run = run
	st := localDispatchRunnerStore{localDispatchStore: ls, row: types.Runner{ID: id, TrustClass: class, OwnerKind: types.RunnerOwnerPerson}}
	srv.cfg.Store = st
	spec := localOrgNetworkSpec(run.ID)
	ok := srv.classifyLocalDispatch(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), sc, &spec, nil, llmTransport{}, false)
	return ok, audit, st, run
}

// P6: a denial added after create refuses the run at dispatch, naming machine_kind with the denial's sentence; a
// denial of another cell, and no denial, do not.
func TestPlacementTrust_DispatchRefusesALaterDenial(t *testing.T) {
	siteWith := func(ds ...types.MachineKindDenial) types.SiteConfig {
		return types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true, MachineKindDenials: ds}}
	}
	if ok, audit, _, run := dispatchOfKind(t, types.MachineLinuxVM, types.RunnerTrustPersonal, types.SiteConfig{}); !ok {
		t.Fatalf("a Linux VM on a personal runner with no list was refused: %s", auditDump(audit.snapshot(), run.ID))
	}
	if ok, audit, _, run := dispatchOfKind(t, types.MachineLinuxVM, types.RunnerTrustPersonal,
		siteWith(types.MachineKindDenial{Trust: types.RunnerTrustOrgCluster, MachineKind: types.MachineLinuxVM})); !ok {
		t.Fatalf("a Linux VM on personal with the denial on org_cluster was refused: %s", auditDump(audit.snapshot(), run.ID))
	}
	ok, audit, st, run := dispatchOfKind(t, types.MachineLinuxVM, types.RunnerTrustPersonal,
		siteWith(types.MachineKindDenial{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM}))
	if ok {
		t.Fatal("a Linux VM on a personal runner was dispatched against a denial of it")
	}
	ev := findAudit(audit.snapshot(), run.ID, "run.dispatch", "failure")
	if ev == nil || !strings.Contains(string(ev.Data), string(placement.ReasonPlacementDenied)) || !strings.Contains(string(ev.Data), `"machine_kind"`) {
		t.Fatalf("no placement_denied refusal naming machine_kind: %s", auditDump(audit.snapshot(), run.ID))
	}
	if !strings.Contains(st.failureHint, "Your organisation does not allow Linux VM sandboxes on personal runners.") {
		t.Fatalf("failure hint %q, want the denial's sentence", st.failureHint)
	}
}

// The macOS floor holds at dispatch, where macOSHostGate does not run: a Mac that is no longer managed_device
// fails the run with runner_trust_required, whatever the list says, and a managed one is not refused by it.
func TestPlacementTrust_DispatchFloorRefusesAMacNoLongerManaged(t *testing.T) {
	for _, class := range []types.RunnerTrustClass{types.RunnerTrustPersonal, types.RunnerTrustOrgCluster} {
		ok, audit, _, run := dispatchOfKind(t, types.MachineMacOSVM, class, types.SiteConfig{})
		if ok {
			t.Fatalf("a macOS VM was dispatched to a %s runner", class)
		}
		ev := findAudit(audit.snapshot(), run.ID, "run.dispatch", "failure")
		if ev == nil || !strings.Contains(string(ev.Data), string(placement.ReasonRunnerTrustRequired)) {
			t.Fatalf("%s: no runner_trust_required refusal: %s", class, auditDump(audit.snapshot(), run.ID))
		}
	}
	if ok, audit, _, run := dispatchOfKind(t, types.MachineMacOSVM, types.RunnerTrustManagedDevice, types.SiteConfig{}); !ok {
		t.Fatalf("a macOS VM on a managed_device runner was refused: %s", auditDump(audit.snapshot(), run.ID))
	}
}

// P6, the revive half: a revive keeps its runner, so a list that now denies the run's kind at its class does not
// refuse it; the macOS floor still does.
func TestPlacementTrust_ReviveKeepsItsRunner(t *testing.T) {
	f := newComponentFixture(t)
	f.srv.cfg.Store = reviveTrustStore{localPlanTestStore{f.st}}
	f.st.siteConfig = types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true, MachineKindDenials: []types.MachineKindDenial{
		{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM}, {Trust: types.RunnerTrustPersonal, MachineKind: types.MachineMacOSVM}}}}
	runnerID := uuid.New()
	revive := func(kind types.MachineKind) *reviveError {
		run := types.AgentRun{ID: uuid.New(), CreatedBy: capSub, Placement: types.PlacementLocal, RunnerID: &runnerID, MachineKind: kind}
		return f.srv.reviveOwnerRecheck(context.Background(), run, &proxy.Config{RunID: run.ID}, types.ActorSystem, "wardynd")
	}
	if ref := revive(types.MachineLinuxVM); ref != nil {
		t.Fatalf("a revive of a Linux VM on a runner whose class the list now denies it at: %+v, want it admitted", ref)
	}
	if ref := revive(types.MachineMacOSVM); ref == nil || ref.reason != string(placement.ReasonRunnerTrustRequired) {
		t.Fatalf("a revive of a macOS VM on a personal runner: %+v, want runner_trust_required", ref)
	}
}

// judgeAtTrust skips a class that denies the kind and answers the next that admits it; with every class denying,
// the refusal is the first class's.
func TestJudgeAtTrust_SkipsADeniedClass(t *testing.T) {
	plan := placement.LocalPlan{Spec: runner.SandboxSpec{Image: "alpine"}, MachineKind: types.MachineLinuxVM,
		MachineKindDenials: []types.MachineKindDenial{{Trust: types.RunnerTrustOrgCluster, MachineKind: types.MachineLinuxVM}}}
	classes := []types.RunnerTrustClass{types.RunnerTrustOrgCluster, types.RunnerTrustManagedDevice, types.RunnerTrustPersonal}
	if got, ref := judgeAtTrust(plan, classes, nil); ref != nil || got != types.RunnerTrustManagedDevice {
		t.Fatalf("judgeAtTrust = %q, %+v, want managed_device", got, ref)
	}
	for _, c := range classes {
		plan.MachineKindDenials = append(plan.MachineKindDenials, types.MachineKindDenial{Trust: c, MachineKind: types.MachineLinuxVM})
	}
	plan.MachineKindDenials = slices.Compact(plan.MachineKindDenials)
	got, ref := judgeAtTrust(plan, classes, nil)
	if got != "" || ref == nil || ref.Reason != placement.ReasonPlacementDenied || !strings.Contains(ref.Sentence(), "organisation cluster runners") {
		t.Fatalf("judgeAtTrust with every class denied = %q, %+v, want the first class's placement_denied", got, ref)
	}
}

// P7: only PUT /runners/settings changes the list. GET states [] when none; an entry outside the closed sets, an
// empty kind and a repeat are refused 400; a valid list is stored, audited (before and after) and survives a later
// write of another field; an empty list clears it.
func TestRunnerSettings_MachineKindDenials(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	f.siteCfg.Runners = &types.RunnerSettings{Enabled: true}
	srv.cfg.ControlPlaneURL = "https://wardynd.internal:8443"
	admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
	put := func(body string) *httptest.ResponseRecorder {
		return doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", admin, body)
	}
	get := func() string {
		return doSSO(t, srv, http.MethodGet, "/api/v1/runners/settings", admin, "").Body.String()
	}
	if body := get(); !strings.Contains(body, `"machine_kind_denials":[]`) {
		t.Fatalf("GET with none stored: %s, want machine_kind_denials []", body)
	}
	const entry = `{"trust_class":"personal","machine_kind":"linux_vm"}`
	for name, body := range map[string]string{
		"unknown class": `{"machine_kind_denials":[{"trust_class":"laptop","machine_kind":"linux_vm"}]}`,
		"unknown kind":  `{"machine_kind_denials":[{"trust_class":"personal","machine_kind":"windows_vm"}]}`,
		"empty kind":    `{"machine_kind_denials":[{"trust_class":"personal","machine_kind":""}]}`,
		"duplicate":     `{"machine_kind_denials":[` + entry + `,` + entry + `]}`,
	} {
		if w := put(body); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonMachineKindDenialsInvalid) || len(f.siteCfg.Runners.MachineKindDenials) != 0 {
			t.Errorf("%s: %d %s, want 400 %s and nothing stored", name, w.Code, w.Body.String(), reasonMachineKindDenialsInvalid)
		}
	}
	if w := put(`{"machine_kind_denials":[` + entry + `]}`); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), entry) {
		t.Fatalf("a valid list: %d %s", w.Code, w.Body.String())
	}
	want := []types.MachineKindDenial{{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM}}
	if rs := f.siteCfg.Runners; rs == nil || !rs.Enabled || !slices.Equal(rs.MachineKindDenials, want) {
		t.Fatalf("stored %+v, want enabled with the list", rs)
	}
	ev := lastAuditEvent(t, h.audit.snapshot(), "runners.enabled.set")
	var data struct{ Before, After []types.MachineKindDenial }
	if err := json.Unmarshal(ev.Data, &data); err != nil || ev.Target != "runners.machine_kind_denials" || len(data.Before) != 0 || !slices.Equal(data.After, want) {
		t.Fatalf("audit %+v data %s, want target runners.machine_kind_denials, before [] and after the list", ev, ev.Data)
	}
	if w := put(`{"enabled":true}`); w.Code != http.StatusOK || !slices.Equal(f.siteCfg.Runners.MachineKindDenials, want) {
		t.Fatalf("an enabled-only write: %d %+v, want the list kept", w.Code, f.siteCfg.Runners)
	}
	if body := get(); !strings.Contains(body, entry) {
		t.Fatalf("GET after the write: %s", body)
	}
	if w := put(`{"machine_kind_denials":[]}`); w.Code != http.StatusOK || len(f.siteCfg.Runners.MachineKindDenials) != 0 {
		t.Fatalf("an empty list: %d %+v, want it cleared", w.Code, f.siteCfg.Runners)
	}
}

// PUT /site-config cannot change the list: a different list is refused, one left out carries the stored list, and
// an echo of it succeeds.
func TestSiteConfigCannotChangeMachineKindDenials(t *testing.T) {
	stored := []types.MachineKindDenial{{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM}}
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true, MachineKindDenials: stored}}}
	srv, _ := newSiteConfigHarness(t, fake)
	const other = `[{"trust_class":"org_cluster","machine_kind":"linux_vm"}]`
	for name, body := range map[string]string{"another list": other, "cleared": `[]`} {
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"runners":{"enabled":true,"machine_kind_denials":`+body+`}}`); w.Code != http.StatusBadRequest ||
			!strings.Contains(w.Body.String(), reasonSiteConfigRunnersViaOwnRoute) || fake.putSeen != nil {
			t.Fatalf("%s: %d %s, want 400 %s and no write", name, w.Code, w.Body.String(), reasonSiteConfigRunnersViaOwnRoute)
		}
	}
	for name, body := range map[string]string{
		"left out": `{"runners":{"enabled":true}}`,
		"an echo":  `{"runners":{"enabled":true,"machine_kind_denials":[{"trust_class":"personal","machine_kind":"linux_vm"}]}}`,
	} {
		fake.putSeen = nil
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body); w.Code != http.StatusOK || fake.putSeen == nil || fake.putSeen.Runners == nil ||
			!slices.Equal(fake.putSeen.Runners.MachineKindDenials, stored) {
			t.Fatalf("%s: %d %s, want 200 with the stored list carried", name, w.Code, w.Body.String())
		}
	}
}
