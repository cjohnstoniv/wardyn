// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// macName is the name macRunner gives its runner: the one the refusals open with.
const macName = "Mac mini"

func readyMac() *runnerwire.MacVMCaps {
	return &runnerwire.MacVMCaps{Max: 2, MinCPUs: 4, MinMemoryMiB: 8192, Harnesses: []string{"none", "claude-code"},
		Desktop: runnerwire.MacVMDesktopControl,
		Base:    runnerwire.MacVMBase{State: runnerwire.MacVMBaseReady, GuestOS: "macOS 27.0.1 (26A434)"}}
}

// macRunner installs a claimed runner of capsOwner whose caps carry mc and whose row is a managed Mac
// (trust managed_device, posture mdm_managed from the MDM file), then lets edit change the row.
func macRunner(t *testing.T, s *Server, mc *runnerwire.MacVMCaps, edit func(*types.Runner)) uuid.UUID {
	t.Helper()
	caps := laptopCaps(types.CC1)
	caps.MacOSVM = mc
	id := installRunner(t, s, true, caps)
	rows := s.cfg.Store.(ownedRunners).rows
	rn := rows[id]
	rn.Name = macName
	rn.TrustClass = types.RunnerTrustManagedDevice
	rn.Posture = types.RunnerPosture{MDMManaged: true, Evidence: types.PostureEvidenceMDMFile}
	if edit != nil {
		edit(&rn)
	}
	rows[id] = rn
	return id
}

func macReq(id uuid.UUID) createRunRequest {
	return createRunRequest{Placement: types.PlacementLocal, RunnerID: id.String(), MachineKind: "macos_vm"}
}

// macLiveStore adds the org's count of macos_vm runs per runner to ownedRunners.
type macLiveStore struct {
	ownedRunners
	held map[uuid.UUID]int
}

func (m macLiveStore) RunnerMacOSVMHolders(context.Context, []uuid.UUID) (map[uuid.UUID]int, error) {
	return m.held, nil
}

// A macOS VM is placed only on a Mac runner whose caps offer it, that the managed gate admits, whose
// base is ready and has room, and whose base serves the run's harness at the run's size. Each refusal is
// placement_capability with its own sentence, and a Linux-only runner is never asked for one.
func TestPlacementCapability_MacOSVM(t *testing.T) {
	operator := withOIDCRole(capsCtx, oidc.RoleSecurityAdmin)
	member := withOIDCRole(capsCtx, oidc.RoleUser)
	mac := func(edit func(*runnerwire.MacVMCaps)) *runnerwire.MacVMCaps {
		mc := readyMac()
		if edit != nil {
			edit(mc)
		}
		return mc
	}
	perRun := func(cpuMillis, memoryMiB int64) func(*types.Runner) {
		return func(r *types.Runner) {
			r.Limits = &types.RunnerLimits{PerRun: types.RunnerAmounts{CPUMillis: cpuMillis, MemoryMiB: memoryMiB}}
		}
	}
	for _, tc := range []struct {
		name    string
		mc      *runnerwire.MacVMCaps
		row     func(*types.Runner)
		ctx     context.Context
		ask     macOSVMAsk
		mounts  int
		want    string // "" means the run is placed
		wantNot string
		reason  placement.Reason // the refusal's code; placement_capability when unset
		field   string           // the request field the refusal names; machine_kind when unset, "-" for none
	}{
		{name: "ready managed Mac", mc: mac(nil), ctx: member},
		{name: "the exact minimum size", mc: mac(nil), ctx: member, ask: macOSVMAsk{CPUMillis: 4000, MemoryMiB: 8192}},
		{name: "your own tools need no harness", mc: mac(func(m *runnerwire.MacVMCaps) { m.Harnesses = nil }), ctx: member, ask: macOSVMAsk{Harnesses: []string{"none"}}},
		{name: "no macos_vm caps", mc: nil, ctx: member, want: remote.MacOSVMNotOffered(macName)},
		{name: "runner's own refusal", mc: mac(func(m *runnerwire.MacVMCaps) { m.Refusal = "needs macOS 27 or later" }), ctx: member,
			want: macOSPoolCannotTake, wantNot: "needs macOS 27"},
		{name: "not marked managed, member", mc: mac(nil), row: func(r *types.Runner) { r.TrustClass = types.RunnerTrustPersonal }, ctx: member,
			want: "Mac mini is not marked as company-managed. Ask a security operator to mark it.", wantNot: "wardyn runner trust"},
		{name: "not marked managed, security operator", mc: mac(nil), row: func(r *types.Runner) { r.TrustClass = types.RunnerTrustPersonal }, ctx: operator,
			want: "Mac mini is not marked as company-managed. Mark it with `wardyn runner trust "},
		{name: "posture from the OS probe", mc: mac(nil), row: func(r *types.Runner) { r.Posture.Evidence = types.PostureEvidenceOSProbe }, ctx: member,
			want: "Mac mini has no MDM posture file saying it is managed (`/Library/Application Support/Wardyn/posture.json`).", reason: placement.ReasonRunnerPostureUnmet},
		{name: "posture not MDM managed", mc: mac(nil), row: func(r *types.Runner) { r.Posture.MDMManaged = false }, ctx: member,
			want: "no MDM posture file", reason: placement.ReasonRunnerPostureUnmet},
		{name: "base preparing", mc: mac(func(m *runnerwire.MacVMCaps) {
			m.Base = runnerwire.MacVMBase{State: runnerwire.MacVMBasePreparing, Progress: 40}
		}), ctx: member, want: "Mac mini is still preparing macOS (40%)."},
		{name: "base needs an update", mc: mac(func(m *runnerwire.MacVMCaps) { m.Base.State = runnerwire.MacVMBaseUpdateNeeded }), ctx: member,
			want: "Mac mini has no macOS base image ready yet."},
		{name: "base failed, with its error", mc: mac(func(m *runnerwire.MacVMCaps) {
			m.Base = runnerwire.MacVMBase{State: runnerwire.MacVMBaseUpdateNeeded, Error: "the download failed"}
		}), ctx: member, want: "Mac mini has no macOS base image ready yet: the download failed."},
		{name: "no room", mc: mac(func(m *runnerwire.MacVMCaps) { m.Max = 0 }), ctx: member, want: "Mac mini has no room for another macOS VM right now."},
		{name: "harness the base cannot serve, tools missing", mc: mac(func(m *runnerwire.MacVMCaps) { m.Harnesses = []string{"none"} }), ctx: member,
			ask: macOSVMAsk{Harnesses: []string{"claude-code"}}, want: "macOS VMs on Mac mini offer only Your own tools until Apple's Command Line Tools finish installing.", field: "harness"},
		{name: "harness the base cannot serve", mc: mac(nil), ctx: member, ask: macOSVMAsk{Harnesses: []string{"claude-code", "codex-cli"}},
			want: "This Mac's macOS base can't run Codex CLI right now.", field: "harness"},
		{name: "memory under the base minimum", mc: mac(nil), ctx: member, ask: macOSVMAsk{CPUMillis: 4000, MemoryMiB: 4096},
			want: "Per-run memory on this runner is 4 GB; a macOS VM needs at least 8 GB. Its owner raises it to 8 GB or more in Your account → My runner, or with `wardyn runner set Mac mini --per-run-memory 8`.", field: "resources"},
		{name: "CPUs under the base minimum", mc: mac(nil), ctx: member, ask: macOSVMAsk{CPUMillis: 2500, MemoryMiB: 8192},
			want: "Per-run CPU on this runner is 2 CPUs; a macOS VM needs at least 4. Its owner raises it to 4 or more in Your account → My runner, or with `wardyn runner set Mac mini --per-run-cpu 4`.", field: "resources"},
		{name: "CPUs under the runner's per-run limit", mc: mac(nil), ctx: member, ask: macOSVMAsk{CPUMillis: 2000, MemoryMiB: 8192},
			row: perRun(3000, 16384), want: "Per-run CPU on this runner is 3 CPUs; a macOS VM needs at least 4. Its owner raises it to 4 or more", field: "resources"},
		{name: "memory under the runner's per-run limit", mc: mac(nil), ctx: member, ask: macOSVMAsk{CPUMillis: 4000, MemoryMiB: 4096},
			row: perRun(8000, 6144), want: "Per-run memory on this runner is 6 GB; a macOS VM needs at least 8 GB. Its owner raises it to 8 GB or more", field: "resources"},
		// The runner gives enough; the run's own ask was lowered under the minimum.
		{name: "CPUs asked under the base minimum", mc: mac(nil), ctx: member, ask: macOSVMAsk{CPUMillis: 2000, MemoryMiB: 8192},
			row: perRun(8000, 16384), want: "A macOS VM needs at least 4 CPU and 8192 MiB.", wantNot: "Its owner raises it", field: "resources"},
		{name: "memory asked under the base minimum", mc: mac(nil), ctx: member, ask: macOSVMAsk{CPUMillis: 4000, MemoryMiB: 4096},
			row: perRun(8000, 16384), want: "A macOS VM needs at least 4 CPU and 8192 MiB.", wantNot: "Its owner raises it", field: "resources"},
		{name: "a folder on a runner too old to share one", mc: mac(nil), ctx: member, mounts: 1, want: runner.MacVMNoFolderShare,
			wantNot: remote.VMNoFolderShare, field: "workspaces"},
		{name: "a folder on a runner that shares folders", mc: mac(func(m *runnerwire.MacVMCaps) { m.Folders = true }), ctx: member, mounts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := capsServer(&fakeRunner{})
			id := macRunner(t, s, tc.mc, tc.row)
			ref := s.placementCapabilityRefusalFor(tc.ctx, macReq(id), runPlacement{}, governanceCeiling{}, tc.mounts, tc.ask)
			if tc.want == "" {
				if ref != nil {
					t.Fatalf("refused: %+v", ref)
				}
				return
			}
			reason := cmp.Or(tc.reason, placement.ReasonPlacementCapability)
			if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Reason != string(reason) ||
				!strings.Contains(ref.body.Error, tc.want) || (tc.wantNot != "" && strings.Contains(ref.body.Error, tc.wantNot)) {
				t.Fatalf("refusal = %+v, want 422 %s naming %q", ref, reason, tc.want)
			}
			// The sentence is the detail alone: the field travels in its own member, never as a prefix.
			if field := ref.body.Field; field != "" && strings.HasPrefix(ref.body.Error, field+":") {
				t.Fatalf("refusal %q opens with its field %q", ref.body.Error, field)
			}
			// The field routes the refusal to the New Run step that changes it.
			if field := strings.TrimPrefix(cmp.Or(tc.field, "machine_kind"), "-"); ref.body.Field != field {
				t.Fatalf("refusal names field %q, want %q", ref.body.Field, field)
			}
		})
	}
}

// A Linux-only runner, the deployment's own executor and an unplaced request never take a macOS VM, and
// a macOS-only runner never takes a Linux one.
func TestPlacementCapability_MacOSVMKindsAreSeparate(t *testing.T) {
	s := capsServer(&fakeRunner{})
	linuxCaps := laptopCaps(types.CC1)
	linuxCaps.LinuxVM = &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Virtiofs: true}
	linuxOnly := installRunner(t, s, true, linuxCaps)
	macOnly := macRunner(t, s, readyMac(), nil)

	for name, req := range map[string]createRunRequest{
		"linux-only runner": macReq(linuxOnly),
		"remote":            {Placement: types.PlacementRemote, MachineKind: "macos_vm"},
		"unplaced":          {MachineKind: "macos_vm"},
	} {
		ref := s.placementCapabilityRefusal(capsCtx, req, runPlacement{}, governanceCeiling{}, 0)
		if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Reason != string(placement.ReasonPlacementCapability) ||
			!strings.Contains(ref.body.Error, "macOS VM") {
			t.Errorf("%s: refusal = %+v, want 422 placement_capability naming macOS VM sandboxes", name, ref)
		}
		if ref != nil && ref.body.Field != "machine_kind" {
			t.Errorf("%s: refusal names field %q, want machine_kind", name, ref.body.Field)
		}
	}
	linuxReq := createRunRequest{Placement: types.PlacementLocal, RunnerID: macOnly.String(), MachineKind: "linux_vm"}
	if ref := s.placementCapabilityRefusal(capsCtx, linuxReq, runPlacement{}, governanceCeiling{}, 0); ref == nil || !strings.Contains(ref.body.Error, "Linux VM") {
		t.Errorf("a Linux VM on a macOS-only runner: refusal = %+v", ref)
	}
	// A Linux VM keeps its folder rule, and a container asks nothing of a Mac.
	linuxReq.RunnerID = linuxOnly.String()
	if ref := s.placementCapabilityRefusal(capsCtx, linuxReq, runPlacement{}, governanceCeiling{}, 1); ref != nil {
		t.Errorf("a Linux VM with a folder on a virtiofs runner was refused: %+v", ref)
	}
	if ref := s.placementCapabilityRefusal(capsCtx, createRunRequest{Placement: types.PlacementLocal, RunnerID: macOnly.String()}, runPlacement{}, governanceCeiling{}, 1); ref != nil {
		t.Errorf("a container on a Mac runner was refused: %+v", ref)
	}
	// A pooled run lands on any eligible runner, so every one of them must offer macOS VMs.
	pooled := runPlacement{pooled: true, placement: types.PlacementLocal, eligible: []poolTarget{{runnerID: &macOnly}, {runnerID: &linuxOnly}}}
	if ref := s.placementCapabilityRefusal(capsCtx, createRunRequest{MachineKind: "macos_vm"}, pooled, governanceCeiling{}, 0); ref == nil || ref.body.Error != macOSPoolNotOffered {
		t.Errorf("a pool with a runner that does not offer macOS VMs: refusal = %+v, want %q", ref, macOSPoolNotOffered)
	}
}

// A pool of several Macs is told the pool's sentence, not one Mac's: that a runner cannot take a macOS
// VM now (macOSPoolCannotTake), or cannot start the harness (the pool form of the Your own tools sentence).
// A pool of one Mac is told that Mac's own sentence.
func TestPlacementCapability_MacOSVMPoolForms(t *testing.T) {
	s := capsServer(&fakeRunner{})
	ready := macRunner(t, s, readyMac(), nil)
	preparing := macRunner(t, s, func() *runnerwire.MacVMCaps {
		mc := readyMac()
		mc.Base = runnerwire.MacVMBase{State: runnerwire.MacVMBasePreparing, Progress: 40}
		return mc
	}(), nil)
	toolsLess := macRunner(t, s, func() *runnerwire.MacVMCaps {
		mc := readyMac()
		mc.Harnesses = []string{"none"}
		return mc
	}(), nil)
	pool := func(ids ...uuid.UUID) runPlacement {
		p := runPlacement{pooled: true, placement: types.PlacementLocal}
		for i := range ids {
			p.eligible = append(p.eligible, poolTarget{runnerID: &ids[i]})
		}
		return p
	}
	req := createRunRequest{MachineKind: "macos_vm"}
	for _, tc := range []struct {
		name  string
		pool  runPlacement
		ask   macOSVMAsk
		field string
		want  string
	}{
		{"one cannot take it now", pool(ready, preparing), macOSVMAsk{}, "machine_kind", macOSPoolCannotTake},
		{"one cannot start the harness", pool(toolsLess, ready), macOSVMAsk{Harnesses: []string{"codex-cli"}}, "harness",
			"Every runner in this pool must offer Codex CLI in a macOS VM. Until Apple's Command Line Tools finish installing on each Mac, choose Your own tools."},
		{"a pool of one Mac", pool(preparing), macOSVMAsk{}, "machine_kind", "Mac mini is still preparing macOS (40%)."},
	} {
		ref := s.placementCapabilityRefusalFor(capsCtx, req, tc.pool, governanceCeiling{}, 0, tc.ask)
		if ref == nil || ref.body.Error != tc.want || ref.body.Field != tc.field {
			t.Errorf("%s: refusal = %+v, want %q", tc.name, ref, tc.want)
		}
	}
	// Every eligible runner of a pooled run must meet its demand: a pool with one Mac that cannot share a
	// folder refuses a run that shares one, and takes a run that shares none.
	sharing := macRunner(t, s, func() *runnerwire.MacVMCaps {
		mc := readyMac()
		mc.Folders = true
		return mc
	}(), nil)
	if ref := s.placementCapabilityRefusalFor(capsCtx, req, pool(sharing, ready), governanceCeiling{}, 1, macOSVMAsk{}); ref == nil ||
		ref.body.Error != runner.MacVMNoFolderShare || ref.body.Field != "workspaces" {
		t.Errorf("a mixed pool and a folder: refusal = %+v, want %q", ref, runner.MacVMNoFolderShare)
	}
	if ref := s.placementCapabilityRefusalFor(capsCtx, req, pool(sharing, ready), governanceCeiling{}, 0, macOSVMAsk{}); ref != nil {
		t.Errorf("a mixed pool and no folder: refused %+v", ref)
	}
	if ref := s.placementCapabilityRefusalFor(capsCtx, req, pool(sharing), governanceCeiling{}, 1, macOSVMAsk{}); ref != nil {
		t.Errorf("a pool that shares folders: refused %+v", ref)
	}
}

// The org's count of macos_vm runs on a Mac is a best-effort guard against caps.max.
func TestPlacementCapability_MacOSVMHonoursItsLiveCount(t *testing.T) {
	for _, tc := range []struct {
		name      string
		max, held int
		want      string
	}{
		{"licence cap", 2, 2, "Mac mini already runs 2 macOS VMs, the most Apple's licence allows."},
		{"cap lowered to one", 1, 1, "Mac mini has no room for another macOS VM right now."},
		{"room left", 2, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := capsServer(&fakeRunner{})
			mc := readyMac()
			mc.Max = tc.max
			id := macRunner(t, s, mc, nil)
			s.cfg.Store = macLiveStore{ownedRunners: s.cfg.Store.(ownedRunners), held: map[uuid.UUID]int{id: tc.held}}
			ref := s.placementCapabilityRefusal(capsCtx, macReq(id), runPlacement{}, governanceCeiling{}, 0)
			if (tc.want == "") != (ref == nil) || (ref != nil && !strings.Contains(ref.body.Error, tc.want)) {
				t.Fatalf("refusal = %+v, want %q", ref, tc.want)
			}
		})
	}
}

// The create fold hands the placement the run's harnesses and the size stepResources applied.
func TestStepPlacementCapability_MacOSVMAskIsTheRunsOwn(t *testing.T) {
	s := capsServer(&fakeRunner{})
	id := macRunner(t, s, readyMac(), nil)
	applied := func(cpuMillis, memMiB int) client.AppliedResources {
		return client.AppliedResources{CPUMillis: client.AppliedAmount{Applied: cpuMillis}, MemoryMiB: client.AppliedAmount{Applied: memMiB}}
	}
	for _, tc := range []struct {
		name      string
		agent     string
		harnesses []types.RunHarness
		applied   client.AppliedResources
		want      string
	}{
		{"agent served, size fits", "claude-code", nil, applied(4000, 8192), ""},
		{"harness set served", "", []types.RunHarness{{Harness: "claude-code", Starts: true}, {Harness: "none"}}, applied(4000, 8192), ""},
		{"agent not served", "codex-cli", nil, applied(4000, 8192), "can't run Codex CLI"},
		{"one harness of a set not served", "claude-code", []types.RunHarness{{Harness: "claude-code"}, {Harness: "codex-cli"}}, applied(4000, 8192), "can't run Codex CLI"},
		{"applied memory under the minimum", "claude-code", nil, applied(4000, 2048), "Per-run memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := macReq(id)
			req.Agent = tc.agent
			w := httptest.NewRecorder()
			f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req, harnesses: tc.harnesses}
			f.place.applied = tc.applied
			ok := s.stepPlacementCapability(f, nil)
			if ok != (tc.want == "") || (!ok && !strings.Contains(w.Body.String(), tc.want)) {
				t.Fatalf("step ok = %v, body %s", ok, w.Body.String())
			}
		})
	}
}

func TestRequestDemands_MacOSVM(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		mounts int
		want   placementDemands
	}{
		{"", 1, placementDemands{}},
		{"linux_vm", 0, placementDemands{LinuxVM: true}},
		{"linux_vm", 2, placementDemands{LinuxVM: true, VMFolders: true}},
		{"macos_vm", 0, placementDemands{MacOSVM: true}},
		{"macos_vm", 2, placementDemands{MacOSVM: true}},
	} {
		if got := requestDemands(createRunRequest{MachineKind: tc.kind}, governanceCeiling{}, tc.mounts); got != tc.want {
			t.Errorf("kind %q with %d folders: demands %+v, want %+v", tc.kind, tc.mounts, got, tc.want)
		}
	}
}

// The managed gate is ordered (trust, then posture), its evidence names what it rested on and says there
// is no certificate check yet, and the trust sentence names the fixing command only to a security operator.
func TestMacOSHostGate(t *testing.T) {
	id := uuid.New()
	managed := types.Runner{ID: id, TrustClass: types.RunnerTrustManagedDevice,
		Posture: types.RunnerPosture{MDMManaged: true, Evidence: types.PostureEvidenceMDMFile}}
	evidence, ref := macOSHostGate(managed, false)
	if ref != nil || len(evidence) != 3 || evidence[0] != "managed_device (set by an administrator)" || evidence[1] != "MDM posture file" ||
		evidence[2] != "no MDM device-certificate check yet" {
		t.Fatalf("managed Mac: evidence %v, refusal %+v", evidence, ref)
	}
	for _, class := range []types.RunnerTrustClass{types.RunnerTrustPersonal, types.RunnerTrustOrgCluster, ""} {
		rn := managed
		rn.TrustClass = class
		if _, ref := macOSHostGate(rn, true); ref == nil || ref.Reason != placement.ReasonPlacementCapability || !strings.Contains(ref.Detail, "wardyn runner trust "+id.String()+" managed_device") {
			t.Errorf("trust %q, operator: refusal = %+v", class, ref)
		}
		if _, ref := macOSHostGate(rn, false); ref == nil || strings.Contains(ref.Detail, "wardyn runner trust") || !strings.Contains(ref.Detail, "Ask a security operator") {
			t.Errorf("trust %q, member: refusal = %+v", class, ref)
		}
	}
	// A missing posture is runner_posture_unmet, not placement_capability: the console keys its words by code.
	for _, posture := range []types.RunnerPosture{{}, {MDMManaged: true, Evidence: types.PostureEvidenceOSProbe}} {
		rn := managed
		rn.Posture = posture
		if _, ref := macOSHostGate(rn, false); ref == nil || ref.Reason != placement.ReasonRunnerPostureUnmet || !strings.Contains(ref.Detail, "no MDM posture file") {
			t.Errorf("posture %+v: refusal = %+v", posture, ref)
		}
	}
	// Trust is checked first: a personal runner with no posture is told about trust.
	rn := types.Runner{ID: id, TrustClass: types.RunnerTrustPersonal}
	if _, ref := macOSHostGate(rn, false); ref == nil || !strings.Contains(ref.Detail, "not marked as company-managed") {
		t.Errorf("trust and posture both missing: refusal = %+v", ref)
	}
}

// macOSVMOffer is the one answer the surfaces read: offered exactly when no refusal, with the caps' max,
// harnesses and desktop, and the gate's evidence.
func TestMacOSVMOffer(t *testing.T) {
	rn := types.Runner{ID: uuid.New(), TrustClass: types.RunnerTrustManagedDevice,
		Posture: types.RunnerPosture{MDMManaged: true, Evidence: types.PostureEvidenceMDMFile}}
	caps := runnerwire.Caps{MacOSVM: readyMac()}
	got := macOSVMOffer(rn, caps, true, 1, false)
	if !got.Offered || got.Refusal != nil || got.Max != 2 || got.Desktop != runnerwire.MacVMDesktopControl || len(got.Evidence) != 3 ||
		!slices.Equal(got.Harnesses, []string{"none", "claude-code"}) {
		t.Fatalf("offer = %+v", got)
	}
	for name, off := range map[string]macOSOffer{
		"never reported": macOSVMOffer(rn, runnerwire.Caps{}, false, 0, false),
		"no macOS caps":  macOSVMOffer(rn, runnerwire.Caps{}, true, 0, false),
		"at max":         macOSVMOffer(rn, caps, true, 2, false),
	} {
		if off.Offered || off.Refusal == nil || off.Refusal.Reason != placement.ReasonPlacementCapability {
			t.Errorf("%s: offer = %+v, want a placement_capability refusal", name, off)
		}
	}
}

// With WARDYN_MACOS_VMS off the closed set of kinds is today's two and every refusal reads as it did
// before macos_vm existed, except that macos_vm itself says the switch is off. With it on, macos_vm is a
// kind and an unknown one is refused naming all three.
func TestMachineKindRefusal(t *testing.T) {
	t.Setenv(envMacOSVMs, "")
	for kind, want := range map[types.MachineKind]string{
		"": "", types.MachineContainer: "", types.MachineLinuxVM: "",
		types.MachineMacOSVM: macOSVMsOff,
		"windows_vm":         `machine_kind "windows_vm" is neither "container" nor "linux_vm"`,
	} {
		if got := machineKindRefusal(kind); got != want {
			t.Errorf("switch off, kind %q: %q, want %q", kind, got, want)
		}
	}
	t.Setenv(envMacOSVMs, "1")
	for _, kind := range []types.MachineKind{"", types.MachineContainer, types.MachineLinuxVM, types.MachineMacOSVM} {
		if got := machineKindRefusal(kind); got != "" {
			t.Errorf("switch on, kind %q refused: %q", kind, got)
		}
	}
	if got := machineKindRefusal("windows_vm"); !strings.Contains(got, `"windows_vm"`) || !strings.Contains(got, `"macos_vm"`) {
		t.Errorf("switch on, unknown kind: %q", got)
	}
}

// Create refuses macos_vm with 400 machine_kind_unknown and the switch-off sentence while the switch is off.
func TestDecodeAndValidateCreateRun_MacOSVMSwitchOff(t *testing.T) {
	t.Setenv(envMacOSVMs, "")
	h := newHarness(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(`{"agent":"claude-code","machine_kind":"macos_vm"}`))
	if _, _, _, _, ok := h.srv.decodeAndValidateCreateRun(rec, req); ok || rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), reasonMachineKindUnknown) || !strings.Contains(rec.Body.String(), macOSVMsOff) {
		t.Fatalf("status %d body %s, want 400 %s naming the switch", rec.Code, rec.Body.String(), reasonMachineKindUnknown)
	}
}

// The switch is read at boot (a bad value exits 2 there), and the setup status carries it only when on,
// so a deployment that leaves it off reads the status it always did.
func TestSetupFeatures_MacOSVMs(t *testing.T) {
	if !slices.Contains(envToggles, envMacOSVMs) {
		t.Fatalf("%s is not in envToggles", envMacOSVMs)
	}
	for env, want := range map[string]bool{"": false, "0": false, "1": true} {
		t.Setenv(envMacOSVMs, env)
		f := setupFeatures(false)
		raw, err := json.Marshal(f)
		if err != nil || f.MacOSVMs != want || strings.Contains(string(raw), "macos_vms") != want {
			t.Errorf("%q: MacOSVMs %v, json %s (%v), want %v and the key only when on", env, f.MacOSVMs, raw, err, want)
		}
	}
}

// A macOS VM run records the machine its runner started only when it is a complete macOS machine; a
// Linux VM's machine is not one, and the reverse.
func TestRecordVMMachine_PerKind(t *testing.T) {
	st := vmMachineStore{ownedRunners: ownedRunners{rows: map[uuid.UUID]types.Runner{}}, got: map[uuid.UUID]types.VMMachine{}}
	s := &Server{cfg: Config{Store: st}}
	linux := &types.VMMachine{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", Accelerator: "kvm", QEMUVersion: "10.0.13"}
	mac := &types.VMMachine{MachineType: types.MacVMMachineType, Accelerator: types.MacVMAccelerator, GuestOS: "macOS 27.0.1 (26A434)", HostOS: "macOS 27.0 (26A400)"}
	macRun := types.AgentRun{ID: uuid.New(), MachineKind: types.MachineMacOSVM}
	linuxRun := types.AgentRun{ID: uuid.New(), MachineKind: types.MachineLinuxVM}

	if err := s.recordVMMachine(context.Background(), macRun, runner.Sandbox{Machine: linux}); err == nil {
		t.Fatal("a macOS VM run took a Linux machine")
	}
	if err := s.recordVMMachine(context.Background(), macRun, runner.Sandbox{Machine: &types.VMMachine{MachineType: types.MacVMMachineType, Accelerator: types.MacVMAccelerator}}); err == nil {
		t.Fatal("a macOS machine with no guest build was accepted")
	}
	if err := s.recordVMMachine(context.Background(), linuxRun, runner.Sandbox{Machine: mac}); err == nil {
		t.Fatal("a Linux VM run took a macOS machine")
	}
	if len(st.got) != 0 {
		t.Fatalf("a refused machine was written: %v", st.got)
	}
	if err := s.recordVMMachine(context.Background(), macRun, runner.Sandbox{Machine: mac}); err != nil || st.got[macRun.ID] != *mac {
		t.Fatalf("macOS VM run: %v, wrote %v", err, st.got)
	}
}

// A macOS VM run's view says the desktop its Mac offers, control or observe, from the runner's caps
// now; it is absent for a Mac with no desktop, a runner with no macOS caps, a run on no runner and a
// Linux VM run.
func TestRunMacOSView(t *testing.T) {
	s := capsServer(&fakeRunner{})
	withDesktop := func(d string) *runnerwire.MacVMCaps { mc := readyMac(); mc.Desktop = d; return mc }
	control := macRunner(t, s, readyMac(), nil)
	observe := macRunner(t, s, withDesktop(runnerwire.MacVMDesktopObserve), nil)
	none := macRunner(t, s, withDesktop(runnerwire.MacVMDesktopNone), nil)
	noMac := macRunner(t, s, nil, nil)
	for _, tc := range []struct {
		name   string
		kind   types.MachineKind
		runner *uuid.UUID
		want   string // "" means no view
	}{
		{"control", types.MachineMacOSVM, &control, runnerwire.MacVMDesktopControl},
		{"observe", types.MachineMacOSVM, &observe, runnerwire.MacVMDesktopObserve},
		{"no desktop", types.MachineMacOSVM, &none, ""},
		{"no macOS caps", types.MachineMacOSVM, &noMac, ""},
		{"no runner", types.MachineMacOSVM, nil, ""},
		{"a Linux VM", types.MachineLinuxVM, &control, ""},
	} {
		got := "(none)"
		if v := s.runMacOSView(types.AgentRun{MachineKind: tc.kind, RunnerID: tc.runner}); v != nil {
			got = v.Desktop
		}
		if want := cmp.Or(tc.want, "(none)"); got != want {
			t.Errorf("%s: desktop %q, want %q", tc.name, got, want)
		}
	}
}
