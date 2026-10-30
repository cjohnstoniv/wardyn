// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// errNoRemoteExecutor is the remote placement's answer when no runner.Runner is
// wired: the caller refuses (fail closed) rather than reading an empty answer
// as "nothing is required".
var errNoRemoteExecutor = errors.New("no executor is wired for the remote placement")

// placementCaps is one placement's answer to "what can you enforce": the real
// capabilities of the executor a run actually gets, never another placement's.
type placementCaps struct {
	runner.Capabilities
	// Source says who vouches for the answer: the organisation's substrate, or a
	// runner that cannot be verified.
	Source types.RunEvidenceSource
	// Capacity and AllowedRoots are the runner's own advertisement (design 3.5);
	// both are zero for the remote placement, whose size is the deployment's.
	Capacity     placement.Capacity
	AllowedRoots []string
	// impl answers which optional runner interfaces the placement implements
	// (design 3.4). Set only for a runner, whose list is closed and short of the
	// deployment executor's; the remote placement is the deployment's own.
	impl any
}

// placementCapabilities answers what the placement a run gets can enforce. The
// remote placement is the deployment's executor, which already leaves runner
// substrates out of its answer; a local one is the claimed runner's last `caps`
// event, so an offline runner, or one that never reported, is a runner_offline
// refusal and never a guessed answer. The error is a *placement.Refusal for a
// placement that cannot answer, anything else for a failed read.
func (s *Server) placementCapabilities(ctx context.Context, p types.Placement, runnerID uuid.UUID) (placementCaps, error) {
	if p == types.PlacementLocal {
		return s.localRunnerCapabilities(ctx, runnerID)
	}
	if s.cfg.Runner == nil {
		return placementCaps{}, errNoRemoteExecutor
	}
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err != nil {
		return placementCaps{}, err
	}
	return placementCaps{Capabilities: caps, Source: types.RunEvidenceSubstrate}, nil
}

// requestCapabilities answers for the placement a request names (a local one
// for the runner it pins), or, while it names none, for the remote placement.
// A pooled run's own candidates answer for it through runPlacement instead.
func (s *Server) requestCapabilities(ctx context.Context, req createRunRequest) (placementCaps, error) {
	if req.Placement != "" {
		return s.placementCapabilities(ctx, req.Placement, requestRunnerID(req))
	}
	remote, err := s.placementCapabilities(ctx, types.PlacementRemote, uuid.Nil)
	if err != nil {
		return placementCaps{}, err
	}
	return intersectCapabilities(remote), nil
}

func (s *Server) localRunnerCapabilities(ctx context.Context, id uuid.UUID) (placementCaps, error) {
	if err := s.callerOwnsRunner(ctx, id); err != nil {
		return placementCaps{}, err
	}
	return s.runnerStreamCapabilities(ctx, id)
}

// runCapabilities is what the substrate a dispatched run lands on can enforce: its bound runner's own
// report for a run on a runner, else the deployment's executor. Create bound the run to its owner's own
// runner (runPlacement.apply), so dispatch asks that runner and no request owner.
func (s *Server) runCapabilities(ctx context.Context, run types.AgentRun) (runner.Capabilities, error) {
	if run.RunnerID == nil {
		return s.cfg.Runner.Capabilities(ctx)
	}
	pc, err := s.runnerStreamCapabilities(ctx, *run.RunnerID)
	return pc.Capabilities, err
}

// runnerStreamCapabilities is the runner's last `caps` event; offline or silent is runner_offline.
func (s *Server) runnerStreamCapabilities(ctx context.Context, id uuid.UUID) (placementCaps, error) {
	offline := &placement.Refusal{Reason: placement.ReasonRunnerOffline, Field: "runner_id",
		Detail: "your runner is offline or has not reported its capabilities yet"}
	s.runnerStreams.subsMu.Lock()
	ep, ok := s.runnerStreams.subs[id]
	s.runnerStreams.subsMu.Unlock()
	if !ok {
		return placementCaps{}, offline
	}
	cs, err := ep.sub.Classes(ctx)
	if errors.Is(err, runner.ErrRunnerOffline) {
		return placementCaps{}, offline
	}
	if err != nil {
		return placementCaps{}, err
	}
	reported, ok := ep.sub.Caps()
	if !ok {
		return placementCaps{}, offline
	}
	return placementCaps{
		Capabilities: runner.Capabilities{
			Driver: ep.sub.Name(), ConfinementClasses: cs.Classes, Resolved: cs.Resolved,
			StructuralEgress: cs.StructuralEgress, NetworkPolicy: cs.NetworkPolicy,
			NetworkPolicyAcknowledged: cs.NetworkPolicyAcknowledged, SessionRecording: cs.SessionRecording,
			UserDrives: cs.UserDrives, ManagedFiles: cs.ManagedFiles,
			EphemeralDiskEnforcement: cs.EphemeralDiskEnforcement, Freeze: cs.Freeze,
			ExecOutputUncaptured: cs.ExecOutputUncaptured,
		},
		Source: types.RunEvidenceRunnerAsserted, Capacity: reported.Capacity, AllowedRoots: reported.AllowedRoots, impl: ep.sub,
	}, nil
}

// callerOwnsRunner is nil when id is a claimed runner of the signed-in person, or a claimed organization
// runner in an organization pool they may use (orgRunnerPermitted), else runner_not_found:
// another person's runner, a revoked or unclaimed one and an unknown id answer alike, before anything is
// read from its stream, so no answer says whether it exists or is online.
func (s *Server) callerOwnsRunner(ctx context.Context, id uuid.UUID) error {
	notFound := &placement.Refusal{Reason: placement.ReasonRunnerNotFound, Field: "runner_id", Detail: "you have no runner with that id"}
	owner := poolOwner(ctx)
	st, ok := s.cfg.Store.(interface {
		GetRunner(context.Context, uuid.UUID) (types.Runner, error)
	})
	if owner == "" || !ok || id == uuid.Nil {
		return notFound
	}
	rn, err := st.GetRunner(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return notFound
	case err != nil:
		return err
	case rn.OwnerKind == types.RunnerOwnerOrg && rn.State == types.RunnerClaimed:
		permitted, err := s.orgRunnerPermitted(ctx, id)
		if err != nil {
			return err
		}
		if !permitted {
			return notFound
		}
	case rn.Owner != owner || rn.State != types.RunnerClaimed:
		return notFound
	}
	return nil
}

// orgRunnerPermitted reports whether an organization runner is a member of an organization pool the
// caller may use (its use policy admits them): the only way a person reaches an org runner.
func (s *Server) orgRunnerPermitted(ctx context.Context, id uuid.UUID) (bool, error) {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return false, nil
	}
	rows, refusal := s.permittedPools(ctx, ps, true)
	if refusal != nil {
		return false, refusal.err
	}
	for _, row := range rows {
		if row.Pool.Scope != types.RunnerPoolOrganization {
			continue
		}
		members, err := ps.ListRunnerPoolMembers(ctx, row.Pool.ID)
		if err != nil {
			return false, err
		}
		if slices.ContainsFunc(members, func(m types.RunnerPoolMember) bool { return m.RunnerID != nil && *m.RunnerID == id }) {
			return true, nil
		}
	}
	return false, nil
}

// intersectCapabilities is what a run can count on while its placement is still
// unresolved (the preview, the profile editor's advisory): only what every
// eligible placement enforces. Classes and capability flags must hold on all of
// them, ephemeral-disk enforcement keeps the weakest word, and a placement a
// runner vouches for makes the whole answer runner-asserted.
func intersectCapabilities(sets ...placementCaps) placementCaps {
	if len(sets) == 0 {
		return placementCaps{}
	}
	out := placementCaps{Source: types.RunEvidenceSubstrate, Capabilities: runner.Capabilities{
		Driver: sets[0].Driver, Resolved: map[types.ConfinementClass]string{}, Freeze: map[types.ConfinementClass]bool{},
		StructuralEgress: true, NetworkPolicy: true, NetworkPolicyAcknowledged: true, SessionRecording: true,
		UserDrives: true, ManagedFiles: true, EphemeralDiskEnforcement: sets[0].EphemeralDiskEnforcement,
	}}
	for _, c := range sets[0].ConfinementClasses {
		if every(sets, func(s placementCaps) bool { return slices.Contains(s.ConfinementClasses, c) }) {
			out.ConfinementClasses = append(out.ConfinementClasses, c)
			out.Resolved[c] = sets[0].Resolved[c]
			out.Freeze[c] = every(sets, func(s placementCaps) bool { return s.Freeze[c] })
		}
	}
	for _, s := range sets {
		out.StructuralEgress = out.StructuralEgress && s.StructuralEgress
		out.NetworkPolicy = out.NetworkPolicy && s.NetworkPolicy
		out.NetworkPolicyAcknowledged = out.NetworkPolicyAcknowledged && s.NetworkPolicyAcknowledged
		out.SessionRecording = out.SessionRecording && s.SessionRecording
		out.UserDrives = out.UserDrives && s.UserDrives
		out.ManagedFiles = out.ManagedFiles && s.ManagedFiles
		out.ExecOutputUncaptured = out.ExecOutputUncaptured || s.ExecOutputUncaptured
		if diskEnforcementRank(s.EphemeralDiskEnforcement) < diskEnforcementRank(out.EphemeralDiskEnforcement) {
			out.EphemeralDiskEnforcement = s.EphemeralDiskEnforcement
		}
		if s.Source == types.RunEvidenceRunnerAsserted {
			out.Source = types.RunEvidenceRunnerAsserted
		}
	}
	return out
}

func every(sets []placementCaps, f func(placementCaps) bool) bool {
	return !slices.ContainsFunc(sets, func(s placementCaps) bool { return !f(s) })
}

// diskEnforcementRank orders the ephemeral-disk words strongest-highest, as the
// orchestrator's own aggregation does: anything else, the empty word included,
// ranks weakest, which is the safe half (never overclaim).
func diskEnforcementRank(e types.StorageEnforcement) int {
	switch e {
	case types.StorageEnforcementFilesystem:
		return 2
	case types.StorageEnforcementEviction:
		return 1
	default:
		return 0
	}
}

// placementDemands are the optional runner interfaces (design 3.4) a request's
// own shape needs. A placement that does not implement one is refused, never
// served with the demand quietly dropped.
type placementDemands struct {
	// IdlePause is a profile's pause_idle_after_sec: it needs the Freezer to pause
	// and the ActivitySampler to see the run go quiet.
	IdlePause bool
	// LinuxVM is machine_kind linux_vm: only a registered runner whose caps offer it starts one.
	LinuxVM bool
	// VMFolders is a Linux VM run that carries a folder mount: its runner must share it into the
	// guest (virtiofs), exactly as a container's bind.
	VMFolders bool
	// MacOSVM is machine_kind macos_vm: only a registered Mac runner whose caps offer it, and that the
	// managed-Mac gate admits, starts one (macOSVMOffer).
	MacOSVM bool
}

func requestDemands(req createRunRequest, ceiling governanceCeiling, mounts int) placementDemands {
	kind := types.MachineKind(req.MachineKind)
	linux := kind == types.MachineLinuxVM
	return placementDemands{IdlePause: ceiling.Limits.PauseIdleAfterSec > 0, LinuxVM: linux, VMFolders: linux && mounts > 0, MacOSVM: kind.IsMacOSVM()}
}

// linuxVMOnRemote is the refusal of a Linux VM on the deployment's own executor.
const linuxVMOnRemote = "Linux VM sandboxes run only on a registered runner that offers them"

// macOSVMOnRemote is the refusal of a macOS VM on the deployment's own executor.
const macOSVMOnRemote = "macOS VMs run only on a registered runner that offers them."

// linuxVMAgentStart is the refusal of interactive_start: agent on a Linux VM, whose terminal is a
// plain shell in this release (no shared session to open in the agent CLI or to take over).
const linuxVMAgentStart = "A Linux VM sandbox's terminal opens a shell, not the agent CLI, in this release: set interactive_start to shell"

// runnerCaps is the runner's own last caps event, which remote.Substrate keeps.
type runnerCaps interface {
	Caps() (runnerwire.Caps, bool)
}

// unsupportedDemand is the placement_capability refusal for a demand the
// placement's optional interfaces cannot meet, or nil.
func (c placementCaps) unsupportedDemand(d placementDemands) *placement.Refusal {
	if c.impl == nil {
		return nil
	}
	if d.LinuxVM {
		rc, ok := c.impl.(runnerCaps)
		var caps runnerwire.Caps
		if ok {
			caps, ok = rc.Caps()
		}
		if !ok || caps.LinuxVM == nil {
			return &placement.Refusal{Reason: placement.ReasonPlacementCapability, Field: "machine_kind",
				Detail: "this runner does not offer Linux VM sandboxes: it is older than 0.10, or its operator has not turned them on"}
		}
		if d.VMFolders && !caps.LinuxVM.Virtiofs {
			return &placement.Refusal{Reason: placement.ReasonPlacementCapability, Field: "machine_kind", Detail: remote.VMNoFolderShare}
		}
	}
	if !d.IdlePause {
		return nil
	}
	_, freezes := c.impl.(runner.Freezer)
	_, samples := c.impl.(runner.ActivitySampler)
	if freezes && samples {
		return nil
	}
	return &placement.Refusal{Reason: placement.ReasonPlacementCapability, Field: "pause_idle_after_sec",
		Detail: fmt.Sprintf("your governance profile pauses idle runs, and %s cannot pause one", c.Driver)}
}

// placementCapabilityRefusal is stepPlacementCapability's answer: nil unless the
// run lands on a runner that cannot answer or cannot meet a demand. A pooled run
// lands on the placement its pool decided and on one of its eligible runners, so
// every one of them must meet the demand; an unpooled one on the runner it names.
func (s *Server) placementCapabilityRefusal(ctx context.Context, req createRunRequest, place runPlacement, ceiling governanceCeiling, mounts int) *runRefusal {
	return s.placementCapabilityRefusalFor(ctx, req, place, ceiling, mounts, macOSVMAsk{})
}

// placementCapabilityRefusalFor is placementCapabilityRefusal with the run's own ask of a macOS VM:
// the harnesses it starts and the size it was given. A zero ask checks neither.
func (s *Server) placementCapabilityRefusalFor(ctx context.Context, req createRunRequest, place runPlacement, ceiling governanceCeiling, mounts int, ask macOSVMAsk) *runRefusal {
	ask.Mounts = mounts
	runners := []uuid.UUID{requestRunnerID(req)}
	p := requestPlacement(req)
	if place.pooled {
		p, runners = place.placement, place.eligibleRunners()
	}
	demands := requestDemands(req, ceiling, mounts)
	if demands.LinuxVM && p != types.PlacementLocal {
		return runError(placement.ReasonPlacementCapability.Status(), string(placement.ReasonPlacementCapability), linuxVMOnRemote)
	}
	if demands.MacOSVM && p != types.PlacementLocal {
		return placementRunRefusal(&placement.Refusal{Reason: placement.ReasonPlacementCapability, Field: "machine_kind"}, macOSVMOnRemote)
	}
	if demands.LinuxVM && requestIsInteractive(req) && req.InteractiveStart == "agent" {
		return runError(placement.ReasonPlacementCapability.Status(), string(placement.ReasonPlacementCapability), linuxVMAgentStart)
	}
	if p != types.PlacementLocal || (!demands.IdlePause && !demands.LinuxVM && !demands.MacOSVM) {
		return nil
	}
	for _, id := range runners {
		pc, err := s.placementCapabilities(ctx, types.PlacementLocal, id)
		var ref *placement.Refusal
		switch {
		case errors.As(err, &ref):
		case err != nil:
			return runServerError("read runner capabilities", err)
		default:
			ref = pc.unsupportedDemand(demands)
			if ref == nil && demands.MacOSVM {
				var rerr error
				if ref, rerr = s.macOSVMRefusal(ctx, id, pc, ask, place.pooled && len(runners) > 1); rerr != nil {
					return runServerError("read macOS VM capability", rerr)
				}
			}
		}
		if ref != nil {
			// The field travels in errorBody.Field, so the sentence is the detail alone.
			return placementRunRefusal(ref, ref.Detail)
		}
	}
	return nil
}

// placementRunRefusal is a placement refusal as the run's refusal, saying msg and naming the request
// field it is about (errorBody.Field) when it names one, so the console takes the person to where
// that field is changed.
func placementRunRefusal(ref *placement.Refusal, msg string) *runRefusal {
	f := runError(ref.Reason.Status(), string(ref.Reason), msg)
	f.body.Field = ref.Field
	return f
}

// macOSVMAsk is what one run asks of a Mac beyond its kind: the harnesses it starts and the CPU and
// memory it was given (stepResources). A zero field is not checked.
type macOSVMAsk struct {
	Harnesses []string
	CPUMillis int
	MemoryMiB int
	// Mounts is how many folders the run shares into the VM.
	Mounts int
	// ShareDrives is the run mounts an admin or team drive whose place is a host folder, which a macOS VM
	// takes as a shared folder; a runner without MacVMCaps.Drives is never asked. Drives is the run
	// mounts any other drive of a person's, or keeps its work folder, which a macOS VM does not take yet.
	// VolumeDrive names a drive that is a Docker or cluster volume (ClusterVolume), which a macOS VM can
	// never open.
	ShareDrives, Drives, ClusterVolume bool
	VolumeDrive                        string
	// Prompts is that a second harness has a starting prompt of its own; Holds that it would hold tool
	// calls for approval. A macOS VM does neither.
	Prompts, Holds bool
	// CPU and Memory say how each size came to be, for the sentence of a size under the minimum; the
	// zero value names no cause. Profile is the governance profile's own (leaf) name and Pool the run's
	// pool's, "" for none.
	CPU, Memory   macOSSizeWhy
	Profile, Pool string
}

// macOSSizeWhy is one field of a run's size as the fold resolved it: what it asked for and got, the
// limit that bound it, and, when governance's member cap did, the setting that cap came from.
type macOSSizeWhy struct {
	Applied client.AppliedAmount
	Limit   types.EffectiveLimit
	Knob    runnerpool.CapKnob
}

// macOSOffer is what one Mac runner offers for macOS VMs now: the one function placement, the runner
// drawer and New Run read, so no surface keeps its own readiness story. Refusal is nil exactly when
// Offered.
type macOSOffer struct {
	Offered   bool
	Refusal   *placement.Refusal
	Max       int
	Harnesses []string
	Desktop   string
	// Why is the refusal's client.MacOSVMWhy* code, for the pool list; "" when Offered. On runner_refused,
	// Refused is its client.MacOSVMRefused* reason, with Have and Need for a size below the minimum.
	Why        string
	Refused    string
	Have, Need int
	// Evidence is what the managed-Mac gate rested on, for the run page's "Where it ran".
	Evidence []string
	// Pool is what a pool of several Macs is told instead of Refusal when this runner is why it cannot
	// take a macOS VM (macOSPoolNotOffered, macOSPoolCannotTake); "" when Refusal stands for a pool too.
	Pool string
}

// What a pool of several Macs is told: a pooled run lands on any eligible runner, so every one of them
// must take it, and the one that cannot is named in its own drawer, not here.
const (
	macOSPoolNotOffered = "Every runner in this pool must offer macOS VMs, and at least one doesn't. Choose a pool of Macs that offer them, or ask an admin to make one."
	macOSPoolCannotTake = "Every runner in this pool must be able to take a macOS VM, and at least one can't right now. Try again later, or choose another pool."
)

// macRunnerName is the name the console shows for a runner (runnerLabel in nav-copy.ts): its own, else
// "Runner" and the first 8 characters of its id.
func macRunnerName(rn types.Runner) string {
	if rn.Name != "" {
		return rn.Name
	}
	return "Runner " + rn.ID.String()[:8]
}

// macOSVMHarnessNone is the harness id for "Your own tools": the base serves it whenever it is ready.
const macOSVMHarnessNone = "none"

func macOSRefusal(field, detail string) *placement.Refusal {
	return &placement.Refusal{Reason: placement.ReasonPlacementCapability, Field: field, Detail: detail}
}

// macOSVMOffer turns a runner's last caps, the managed-Mac gate and the macOS VMs already running on it
// into one answer. haveCaps is false for a runner that never sent caps; live is the macos_vm runs the
// org counts on it, a best-effort guard (the runner's own ledger is the authority); operator is whether
// the asker is a security operator, who is told the command that fixes the trust refusal.
func macOSVMOffer(rn types.Runner, caps runnerwire.Caps, haveCaps bool, live int, operator bool) macOSOffer {
	mc := caps.MacOSVM
	if !haveCaps || mc == nil {
		return macOSOffer{Refusal: macOSRefusal("machine_kind", remote.MacOSVMNotOffered(macRunnerName(rn))), Why: client.MacOSVMWhyNotOffered, Pool: macOSPoolNotOffered}
	}
	out := macOSOffer{Max: mc.Max, Harnesses: mc.Harnesses, Desktop: mc.Desktop}
	if mc.Refusal != "" {
		out.Why = client.MacOSVMWhyRunnerRefused
		out.Refused, out.Have, out.Need, out.Refusal = macOSRunnerRefused(rn, mc)
		return out
	}
	evidence, refusal := macOSHostGate(rn, operator)
	if refusal != nil {
		out.Refusal, out.Why = refusal, client.MacOSVMWhyNotManaged
		if refusal.Reason == placement.ReasonRunnerPostureUnmet {
			out.Why = client.MacOSVMWhyNoPosture
		}
		return out
	}
	out.Evidence = evidence
	name := macRunnerName(rn)
	switch {
	case mc.Base.State == runnerwire.MacVMBasePreparing:
		out.Refusal, out.Why = macOSRefusal("machine_kind", fmt.Sprintf("%s is still preparing macOS (%d%%).", name, mc.Base.Progress)), client.MacOSVMWhyPreparing
	case mc.Base.State != runnerwire.MacVMBaseReady:
		detail := name + " has no macOS base image ready yet."
		if mc.Base.Error != "" {
			detail = fmt.Sprintf("%s has no macOS base image ready yet: %s.", name, mc.Base.Error)
		}
		out.Refusal, out.Why = macOSRefusal("machine_kind", detail), client.MacOSVMWhyNoBase
	case live >= mc.Max && mc.Max >= 2:
		out.Refusal, out.Why = macOSRefusal("machine_kind", name+" already runs 2 macOS VMs, the most Apple's licence allows."), client.MacOSVMWhyAtLimit
	case live >= mc.Max:
		out.Refusal, out.Why = macOSRefusal("machine_kind", name+" has no room for another macOS VM right now."), client.MacOSVMWhyNoRoom
	}
	if out.Refusal != nil {
		out.Pool = macOSPoolCannotTake
	}
	out.Offered = out.Refusal == nil
	return out
}

// macOSRunnerRefused is why a runner refuses macOS VMs itself, as the organisation reads it from the
// runner's per-run limits and the base's minimums its caps carry, never from the runner's own text:
// memory below the minimum (MV-R47), CPU below it (MV-R48), else MV-N14's "can't right now".
func macOSRunnerRefused(rn types.Runner, mc *runnerwire.MacVMCaps) (reason string, have, need int, ref *placement.Refusal) {
	if rn.Limits != nil {
		perRun := rn.Limits.PerRun
		if mc.MinMemoryMiB > 0 && int(perRun.MemoryMiB) < mc.MinMemoryMiB {
			have, need = int(perRun.MemoryMiB/1024), (mc.MinMemoryMiB+1023)/1024
			return client.MacOSVMRefusedBelowMinMemory, have, need, macOSRefusal("machine_kind", macOSMemoryBelowMinimum(rn, have, need))
		}
		if mc.MinCPUs > 0 && int(perRun.CPUMillis/1000) < mc.MinCPUs {
			have, need = int(perRun.CPUMillis/1000), mc.MinCPUs
			return client.MacOSVMRefusedBelowMinCPU, have, need, macOSRefusal("machine_kind", macOSCPUBelowMinimum(rn, have, need))
		}
	}
	return client.MacOSVMRefusedOther, 0, 0, macOSRefusal("machine_kind", macOSPoolCannotTake)
}

// macOSCPUBelowMinimum is MV-R48: the runner's per-run CPU is under the base's minimum, which only its
// owner raises.
func macOSCPUBelowMinimum(rn types.Runner, have, need int) string {
	return fmt.Sprintf("Per-run CPU on this runner is %d CPUs; a macOS VM needs at least %d. Its owner raises it to %d or more in Settings → My runner, or with `wardyn runner set %s --per-run-cpu %d`.", have, need, need, macRunnerName(rn), need)
}

// macOSMemoryBelowMinimum is MV-R47, in whole GB.
func macOSMemoryBelowMinimum(rn types.Runner, haveGB, needGB int) string {
	return fmt.Sprintf("Per-run memory on this runner is %d GB; a macOS VM needs at least %d GB. Its owner raises it to %d GB or more in Settings → My runner, or with `wardyn runner set %s --per-run-memory %d`.", haveGB, needGB, needGB, macRunnerName(rn), needGB)
}

// macOSHostGate is the managed-Mac gate (design 3.9): the runner's trust class is managed_device, which
// only a security operator sets, and its posture says mdm_managed from the MDM's file. It returns what
// the pass rested on, or the first check that failed. No device-certificate check exists in 0.10.0.
func macOSHostGate(rn types.Runner, operator bool) (evidence []string, refusal *placement.Refusal) {
	if rn.TrustClass != types.RunnerTrustManagedDevice {
		detail := macRunnerName(rn) + " is not marked as company-managed. Ask a security operator to mark it."
		if operator {
			detail = fmt.Sprintf("%s is not marked as company-managed. Mark it with `wardyn runner trust %s managed_device`.", macRunnerName(rn), rn.ID)
		}
		return nil, macOSRefusal("machine_kind", detail)
	}
	if !rn.Posture.MDMManaged || rn.Posture.Evidence != types.PostureEvidenceMDMFile {
		// runner_posture_unmet, not placement_capability: the console keys its copy by reason code.
		return nil, &placement.Refusal{Reason: placement.ReasonRunnerPostureUnmet, Field: "machine_kind",
			Detail: macRunnerName(rn) + " has no MDM posture file saying it is managed (`/Library/Application Support/Wardyn/posture.json`)."}
	}
	return []string{"managed_device (set by an administrator)", "MDM posture file", "no MDM device-certificate check yet"}, nil
}

// macOSVMRefusal is the refusal (placement_capability, or runner_posture_unmet from the gate) of a macOS VM run on runner id, or nil when the
// Mac offers it and the run's harnesses and size fit what the Mac's base can give.
func (s *Server) macOSVMRefusal(ctx context.Context, id uuid.UUID, pc placementCaps, ask macOSVMAsk, pool bool) (*placement.Refusal, error) {
	var caps runnerwire.Caps
	var haveCaps bool
	if rc, ok := pc.impl.(runnerCaps); ok {
		caps, haveCaps = rc.Caps()
	}
	var rn types.Runner
	if st, ok := s.cfg.Store.(interface {
		GetRunner(context.Context, uuid.UUID) (types.Runner, error)
	}); ok {
		var err error
		if rn, err = st.GetRunner(ctx, id); err != nil {
			return nil, err
		}
	}
	rn.ID = id
	var live int
	if st, ok := s.cfg.Store.(interface {
		RunnerMacOSVMHolders(context.Context, []uuid.UUID) (map[uuid.UUID]int, error)
	}); ok && haveCaps {
		held, err := st.RunnerMacOSVMHolders(ctx, []uuid.UUID{id})
		if err != nil {
			return nil, err
		}
		live = held[id]
	}
	offer := macOSVMOffer(rn, caps, haveCaps, live, s.isSecurityOperator(ctx))
	if !offer.Offered {
		if pool && offer.Pool != "" {
			return macOSRefusal(offer.Refusal.Field, offer.Pool), nil
		}
		return offer.Refusal, nil
	}
	return macOSVMFit(caps.MacOSVM, rn, ask, pool), nil
}

// macOSVMFit refuses a run that shares a folder or a drive on a runner that cannot, whose harness the
// Mac's base cannot serve, or whose applied size is under the base's minimum guest; runnerd checks the
// harness and size again at start, against its own manifest.
func macOSVMFit(mc *runnerwire.MacVMCaps, rn types.Runner, ask macOSVMAsk, pool bool) *placement.Refusal {
	switch {
	case ask.VolumeDrive != "" && ask.ClusterVolume:
		return macOSRefusal("drives", driveMacOSClusterVolumeMsg(ask.VolumeDrive))
	case ask.VolumeDrive != "":
		return macOSRefusal("drives", driveMacOSDockerVolumeMsg(ask.VolumeDrive))
	case ask.Drives:
		return macOSRefusal("drives", macOSVMNoDrives)
	case ask.ShareDrives && !mc.Drives:
		return macOSRefusal("drives", macOSVMNoDriveShare)
	case ask.Prompts:
		return macOSRefusal("tools", macOSVMOneAgent)
	case ask.Holds:
		return macOSRefusal("tools", macOSVMNoHold)
	}
	if ask.Mounts > 0 && !mc.Folders {
		return macOSRefusal("workspaces", runner.MacVMNoFolderShare)
	}
	for _, h := range ask.Harnesses {
		if h == macOSVMHarnessNone || slices.Contains(mc.Harnesses, h) {
			continue
		}
		display := h
		if def, ok := harnessByID(h); ok {
			display = def.Display
		}
		if !slices.ContainsFunc(mc.Harnesses, func(o string) bool { return o != macOSVMHarnessNone }) {
			if pool {
				return macOSRefusal("harness", fmt.Sprintf("Every runner in this pool must offer %s in a macOS VM. Until Apple's Command Line Tools finish installing on each Mac, choose Your own tools.", display))
			}
			return macOSRefusal("harness", fmt.Sprintf("macOS VMs on %s offer only Your own tools until Apple's Command Line Tools finish installing.", macRunnerName(rn)))
		}
		return macOSRefusal("harness", fmt.Sprintf("This Mac's macOS base can't run %s right now.", display))
	}
	cpuLow := ask.CPUMillis > 0 && ask.CPUMillis/1000 < mc.MinCPUs
	memLow := ask.MemoryMiB > 0 && ask.MemoryMiB < mc.MinMemoryMiB
	// The runner's per-run limit is the cause when it too is under the minimum, or unknown: only its owner
	// raises it. Otherwise the run's own ask was lowered under the minimum, and the run raises it.
	cpus, mem := ask.CPUMillis/1000, ask.MemoryMiB
	if rn.Limits != nil {
		cpus, mem = int(rn.Limits.PerRun.CPUMillis/1000), int(rn.Limits.PerRun.MemoryMiB)
	}
	if cpuLow && (rn.Limits == nil || cpus < mc.MinCPUs) {
		return macOSRefusal("resources", macOSCPUBelowMinimum(rn, cpus, mc.MinCPUs))
	}
	if memLow && (rn.Limits == nil || mem < mc.MinMemoryMiB) {
		return macOSRefusal("resources", macOSMemoryBelowMinimum(rn, mem/1024, (mc.MinMemoryMiB+1023)/1024))
	}
	if cpuLow || memLow {
		return macOSRefusal("resources", macOSSizeLow(mc, ask, cpuLow, memLow))
	}
	return nil
}

// What a macOS VM does not do in this release, said at create and Review as the guest launcher says it at
// start (MAC-F01, MAC-A05, MAC-A06). Every rule that reaches the hold lane holds, Run it and Block it
// included, so macOSVMNoHold names no rule to change to.
const (
	macOSVMNoDrives = "A macOS VM sandbox has no drives in this release."
	macOSVMOneAgent = "A macOS VM starts one agent per run in this release, and another harness in this run has its own starting prompt. Remove that prompt, or choose a container or a Linux VM."
	macOSVMNoHold   = "A macOS VM can't hold a tool call for approval in this release, and this run's tool rules or tool approvals would. Change them, or choose a container or a Linux VM."
)

// macOSVMNoDriveShare is MAC-D13: the runner predates drives in a macOS VM. A Docker or cluster volume's
// refusal is driveMacOSDockerVolumeMsg's and driveMacOSClusterVolumeMsg's.
const macOSVMNoDriveShare = "This runner's version can't attach drives to a macOS VM. Update the runner, or run without drives."

// askDrives sorts a run's drives into what a macOS VM asks of its Mac: an admin or team drive in a host
// folder is a shared folder (ShareDrives), a Docker or cluster volume is never opened (VolumeDrive, the
// first), and a person's drive, the kept work folder or a drive of no known place waits for a disk
// (Drives). A drive with a second place mounts from it on the runner the second place is bound to.
func (a *macOSVMAsk) askDrives(drives []runDrive, keep *keptWorkFolder) {
	a.Drives = keep != nil
	for _, d := range drives {
		drive := d.resolved.Drive
		if drive.AltRunnerID != nil {
			drive = drive.SecondPlace()
		}
		switch {
		case drive.OwnerSubject != "":
			a.Drives = true
		case drive.Backend.RunnerTarget() == "k8s" || drive.Backend == types.DriveBackendDockerVolume:
			if a.VolumeDrive == "" {
				a.VolumeDrive, a.ClusterVolume = drive.Name, drive.Backend.RunnerTarget() == "k8s"
			}
		case drive.Backend == types.DriveBackendHostPath:
			a.ShareDrives = true
		default:
			a.Drives = true
		}
	}
}

// macOSVMHolds reports, for a run that starts agents unattended, whether it would launch the hold lane
// (WARDYN_TOOL_APPROVALS=hold), which the macOS guest launcher refuses. It is the trigger applyToolPosture uses for
// the lane (tool approvals already hold, a person's tool rules, a policy rule that holds or blocks); a
// hold only autonomy or the policy's inner walls derive is stepMacOSHold's, later in the fold.
// An interactive run, an exec run and one whose agents have no hold lane never reach it.
func macOSVMHolds(req createRunRequest, spec types.RunPolicySpec) bool {
	if requestIsInteractive(req) || req.TaskMode == "exec" || !slices.ContainsFunc(taskAgents(req), agentHasHoldLane) {
		return false
	}
	return req.ToolApprovals == "hold" || slices.ContainsFunc(req.Tools, hasPersonalToolRules) ||
		slices.ContainsFunc(spec.ToolRules, func(r types.ToolRule) bool { return r.Effect == types.ToolHold || r.Effect == types.ToolDeny })
}

// macOSVMAskOf is the macOS VM ask of a run: its harness ids (the request's agent when it names no
// harness set) and the size stepResources applied, with how each field of it came to be.
func macOSVMAskOf(place runPlacement, ceiling governanceCeiling, harnesses []types.RunHarness, agent string) macOSVMAsk {
	applied := place.applied
	ask := macOSVMAsk{CPUMillis: applied.CPUMillis.Applied, MemoryMiB: applied.MemoryMiB.Applied}
	size := ceiling.authoredSize()
	ask.CPU = macOSSizeWhy{Applied: applied.CPUMillis, Limit: place.limits.CPUMillis,
		Knob: governanceKnob(place.limits.CPUMillis, size.CPUMillis, ceiling.Limits.MaxCPUMillis)}
	ask.Memory = macOSSizeWhy{Applied: applied.MemoryMiB, Limit: place.limits.MemoryMiB,
		Knob: governanceKnob(place.limits.MemoryMiB, size.MemoryMiB, ceiling.Limits.MaxMemoryMiB)}
	if ceiling.Unit != nil {
		ask.Profile = ceiling.Unit.Name
	}
	if place.pooled {
		ask.Pool = place.row.Name
	}
	for _, h := range harnesses {
		ask.Harnesses = append(ask.Harnesses, h.Harness)
	}
	if len(ask.Harnesses) == 0 && agent != "" {
		ask.Harnesses = []string{agent}
	}
	return ask
}

// governanceKnob is the setting governance's member cap on l came from (runnerpool.MemberCapKnob), "" when
// governance did not bound l.
func governanceKnob(l types.EffectiveLimit, ceiling, profileMax int) runnerpool.CapKnob {
	if l.CapSource != types.LimitSourceGovernance || l.Cap.Unlimited {
		return ""
	}
	return runnerpool.MemberCapKnob(ceiling, profileMax, l.Cap.Value)
}

// sizeCauseKind is what lowered one field of a macOS VM's size under the base's minimum.
type sizeCauseKind int

const (
	sizeOwn      sizeCauseKind = iota // the run asked for less itself, or the fold cannot say
	sizeCeiling                       // governance's ceiling size
	sizeLimits                        // governance's profile maximum
	sizeBaseline                      // governance holds the run to the size its policy has
	sizePool                          // the pool's per-run limit
	sizeServer                        // the deployment's limits
	sizeDefault                       // nothing capped it: the default size it got is under the minimum
)

// sizeCause is a kind with the name it is told in: the profile, the pool, or for sizeDefault the whole
// "from ..." clause.
type sizeCause struct {
	kind sizeCauseKind
	name string
}

// sizeLow is one field under the minimum: what the run gets of it, and the two settings its sentence
// names (the ceiling's size and the profile's maximum).
type sizeLow struct {
	cause              sizeCause
	got, resource, max string
}

// macOSSizeLow is the refusal of a size under the base's minimum when no runner limit is why (MAC-Z01 to
// Z08): the minimum, what the run gets, what lowered it and the setting an admin raises. Fields with one
// cause share a sentence; two causes get one each. A cause the fold cannot name leaves the plain minimum.
func macOSSizeLow(mc *runnerwire.MacVMCaps, ask macOSVMAsk, cpuLow, memLow bool) string {
	var lows []sizeLow
	if cpuLow {
		lows = append(lows, sizeLow{cause: ask.CPU.cause(ask, mc.MinCPUs, 1000), got: fmt.Sprintf("%g CPU", float64(ask.CPUMillis)/1000),
			resource: "`resources.cpu_millis`", max: "`limits.max_cpu_millis`"})
	}
	if memLow {
		lows = append(lows, sizeLow{cause: ask.Memory.cause(ask, mc.MinMemoryMiB, 1), got: fmt.Sprintf("%d MiB", ask.MemoryMiB),
			resource: "`resources.memory_mib`", max: "`limits.max_memory_mib`"})
	}
	need := fmt.Sprintf("A macOS VM needs at least %d CPU and %d MiB.", mc.MinCPUs, mc.MinMemoryMiB)
	var sentences []string
	for len(lows) > 0 {
		group, rest := []sizeLow{lows[0]}, []sizeLow(nil)
		for _, o := range lows[1:] {
			if o.cause == group[0].cause {
				group = append(group, o)
			} else {
				rest = append(rest, o)
			}
		}
		lows = rest
		if group[0].cause.kind != sizeOwn {
			sentences = append(sentences, group[0].cause.sentence(group))
		}
	}
	if len(sentences) == 0 {
		return need
	}
	return need + " " + strings.Join(sentences, " ")
}

// cause is why w is under least, in units of the minimum (a CPU is 1000 millis): the limit that capped it
// when that cap is itself under the minimum, else the default it got when it asked for none.
func (w macOSSizeWhy) cause(ask macOSVMAsk, least, unit int) sizeCause {
	l := w.Limit
	if l.CapSource != "" && !l.Cap.Unlimited && l.Cap.Value/unit < least {
		switch {
		case l.CapSource == types.LimitSourceGovernance && ask.Profile != "":
			switch w.Knob {
			case runnerpool.CapKnobCeiling:
				return sizeCause{sizeCeiling, ask.Profile}
			case runnerpool.CapKnobLimits:
				return sizeCause{sizeLimits, ask.Profile}
			case runnerpool.CapKnobBaseline:
				return sizeCause{sizeBaseline, ask.Profile}
			}
		case l.CapSource == types.LimitSourcePool && ask.Pool != "":
			return sizeCause{sizePool, ask.Pool}
		case l.CapSource == types.LimitSourceDeployment:
			return sizeCause{kind: sizeServer}
		}
		return sizeCause{}
	}
	if w.Applied.Requested > 0 {
		return sizeCause{}
	}
	switch {
	case w.Applied.Source == types.LimitSourceGovernance && ask.Profile != "":
		return sizeCause{sizeDefault, "from your governance profile " + ask.Profile}
	case w.Applied.Source == types.LimitSourcePool && ask.Pool != "":
		return sizeCause{sizeDefault, "from the pool " + ask.Pool}
	case w.Applied.Source == types.LimitSourceDeployment:
		return sizeCause{sizeDefault, "on this server"}
	}
	return sizeCause{}
}

// sentence is the rest of the refusal for the fields of one cause: what the run gets, what lowered it
// and the setting that raises it.
func (c sizeCause) sentence(group []sizeLow) string {
	var got, resources, maxes []string
	for _, l := range group {
		got, resources, maxes = append(got, l.got), append(resources, l.resource), append(maxes, l.max)
	}
	gets := "This run gets " + strings.Join(got, " and ")
	that, only := "that", "lowers it"
	if len(group) > 1 {
		that, only = "those", "lower them"
	}
	switch c.kind {
	case sizeCeiling:
		return fmt.Sprintf("%s: your governance profile %s caps it with its ceiling's %s, set on it or on a profile it inherits from. An admin raises %s; %s only %s.",
			gets, c.name, strings.Join(resources, " and "), that, strings.Join(maxes, " and "), only)
	case sizeLimits:
		return fmt.Sprintf("%s: your governance profile %s caps it with %s. An admin raises %s.", gets, c.name, strings.Join(maxes, " and "), that)
	case sizeBaseline:
		return fmt.Sprintf("%s: your governance profile %s keeps a run to the size its policy sets, or to the default size when it sets none. An admin raises %s with the policy's %s.",
			gets, c.name, that, strings.Join(resources, " and "))
	case sizePool:
		return fmt.Sprintf("%s: the pool %s caps each run at that. An admin raises the pool's per-run limits.", gets, c.name)
	case sizeServer:
		return gets + ": this server's limits cap it. Ask an operator to raise them."
	}
	return fmt.Sprintf("%s, the default size %s. Ask for at least the minimum in the run's `resources`, or set it on Review.", gets, c.name)
}
