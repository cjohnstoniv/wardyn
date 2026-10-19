// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
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
}

func requestDemands(req createRunRequest, ceiling governanceCeiling) placementDemands {
	return placementDemands{IdlePause: ceiling.Limits.PauseIdleAfterSec > 0, LinuxVM: types.MachineKind(req.MachineKind).IsVM()}
}

// linuxVMOnRemote is the refusal of a Linux VM on the deployment's own executor.
const linuxVMOnRemote = "Linux VM sandboxes run only on a registered runner that offers them"

// linuxVMAgentStart is the refusal of interactive_start: agent on a Linux VM, whose terminal is a
// plain shell in this release (no shared session to open in the agent CLI or to take over).
const linuxVMAgentStart = "a Linux VM sandbox's terminal opens a shell, not the agent CLI, in this release: set interactive_start to shell"

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
func (s *Server) placementCapabilityRefusal(ctx context.Context, req createRunRequest, place runPlacement, ceiling governanceCeiling) *runRefusal {
	runners := []uuid.UUID{requestRunnerID(req)}
	p := requestPlacement(req)
	if place.pooled {
		p, runners = place.placement, place.eligibleRunners()
	}
	demands := requestDemands(req, ceiling)
	if demands.LinuxVM && p != types.PlacementLocal {
		return runError(placement.ReasonPlacementCapability.Status(), string(placement.ReasonPlacementCapability), linuxVMOnRemote)
	}
	if demands.LinuxVM && requestIsInteractive(req) && req.InteractiveStart == "agent" {
		return runError(placement.ReasonPlacementCapability.Status(), string(placement.ReasonPlacementCapability), linuxVMAgentStart)
	}
	if p != types.PlacementLocal || (!demands.IdlePause && !demands.LinuxVM) {
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
		}
		if ref != nil {
			return runError(ref.Reason.Status(), string(ref.Reason), ref.Error())
		}
	}
	return nil
}
