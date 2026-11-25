// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A move's destination sandbox. Dispatch builds a run's spec from values that exist only while it dispatches, and a
// revive never builds one, so the agent half of the spec is recorded at dispatch (run_sandbox_specs) and the
// destination is built from that record: the agent half, created and not started, with no proxy process and no proxy
// config. The destination's policy comes only from the sealed revive that starts it after commit.

// moveDestRun is the run with the destination's placement, runner and pool columns, in memory only: nothing is
// written to the run row before commit.
func moveDestRun(run types.AgentRun, d moveDest, executor string) types.AgentRun {
	out := run
	out.Placement, out.RunnerID = d.Placement, d.RunnerID
	out.RunnerPoolID, out.RunnerPoolRevision, out.RunnerPoolSelection = d.PoolID, d.PoolRevision, d.PoolSelection
	out.RunnerPoolExecutor = d.Executor
	if executor != "" {
		out.RunnerPoolExecutor = executor
	}
	return out
}

// moveSandboxSpec is the destination's spec from the record: its agent half and nothing else, with the drives the
// destination re-attaches (moveDestDrives) in place of the recorded ones. A runner destination gets no organisation
// component config: the strip dispatch applies to every local plan. Claude Code's managed settings are left out where
// the destination delivers no managed files and the run has no tool rules (dispatch's fallback).
func moveSandboxSpec(run types.AgentRun, rec runSandboxSpecRecord, d moveDest, plan moveDestPlan) runner.SandboxSpec {
	spec := runner.SandboxSpec{
		RunID: run.ID, Image: rec.Image, ImageArches: slices.Clone(rec.ImageArches), ConfinementClass: rec.ConfinementClass,
		MachineKind: rec.MachineKind, Env: maps.Clone(rec.Env), Mounts: slices.Clone(rec.Mounts), Drives: slices.Clone(plan.Drives),
		UserMountRoots: slices.Clone(rec.UserMountRoots), UserWritable: rec.UserWritable, Interactive: rec.Interactive,
		Resources: rec.Resources,
		// The run's labels, as dispatch sets them: a Kubernetes destination finds its pod by them.
		Labels: map[string]string{"wardyn.run": run.ID.String(), "wardyn.agent": run.Agent},
	}
	if d.local() {
		for _, key := range rec.OrgConfigKeys {
			delete(spec.Env, key)
		}
	}
	for _, f := range rec.ManagedFiles {
		if plan.DropSettings && f.Path == rec.AgentPolicy.Path {
			continue
		}
		spec.ManagedFiles = append(spec.ManagedFiles, f)
	}
	return spec
}

// moveDestBuild is a destination made ready to import into: its spec on its target, its lease sent and its
// create-time identity minted.
type moveDestBuild struct {
	destRun  types.AgentRun
	spec     runner.SandboxSpec
	executor string
	jti      string
}

// buildMoveDestination readies the destination (steps 5.2 to 5.5): the spec from the record, with the move's new work
// drive when it made one, on the destination's target (fillSandboxTarget, create's own target rules), the lease the
// destination runner's create needs, and the destination's identity, whose jti the move records for commit and for a
// failed move's revoke. The token itself goes to no runner and no process. The agent policy row says what the
// destination's managed settings are.
func (s *Server) buildMoveDestination(ctx context.Context, x *moveExec) (moveDestBuild, *moveEnd) {
	destRun := moveDestRun(x.run, x.dest, "")
	spec := moveSandboxSpec(x.run, x.rec, x.dest, x.plan)
	if x.workMount != nil {
		spec.Drives = append(spec.Drives, *x.workMount)
	}
	executor, err := s.fillSandboxTarget(ctx, destRun, &spec)
	if err != nil {
		return moveDestBuild{}, failMove(reasonMoveCreateFailed, "The destination's sandbox could not be targeted: "+err.Error())
	}
	if _, err := s.leaseRunOnRunner(ctx, destRun); err != nil {
		return moveDestBuild{}, failMove(reasonMoveLeaseRefused, "The destination runner refused the moved run's lease: "+err.Error())
	}
	id, rerr := s.reviveMintIdentity(ctx, destRun)
	if rerr != nil {
		return moveDestBuild{}, failMove(reasonMoveIdentityRefused, "The destination's identity could not be minted: "+rerr.msg)
	}
	if x.rec.AgentPolicy.Path != "" {
		ap := x.rec.AgentPolicy
		s.auditAgentPolicy(ctx, destRun, runAgentPolicy{path: ap.Path, bytes: ap.Bytes, hold: ap.Hold, locked: ap.Locked,
			toolRules: ap.ToolRules, withheld: x.plan.SettingsWithheld}, spec)
	}
	return moveDestBuild{destRun: destRun, spec: spec, executor: executor, jti: id.JTI}, nil
}

// moveExactImporter is an importer that can import on one named executor of this deployment, as an
// ExactExecutor creates on one.
type moveExactImporter interface {
	ImportSandboxOn(ctx context.Context, executor string, spec runner.SandboxSpec, set runner.CarriedSet, maxBytes int64, carried io.Reader) (runner.Sandbox, runner.CarriedManifest, error)
}

// createMoveDestination is the destination's create (step 5.6), the one caller of ImportSandbox: the agent half from
// the record, not started, with no proxy process and no proxy config (the importer refuses one), filled from carried
// beneath set's roots under the move's own byte bound. It routes by the executor its target named, as a create does.
func (s *Server) createMoveDestination(ctx context.Context, b moveDestBuild, set runner.CarriedSet, maxBytes int64, carried io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
	if b.executor != "" {
		exact, ok := s.cfg.Runner.(moveExactImporter)
		if !ok {
			// Never fall back to the default importer: it would choose a substrate other than the one named.
			return runner.Sandbox{}, runner.CarriedManifest{}, fmt.Errorf("%w: %w: no importer for executor %s", runner.ErrMoveCreate,
				runner.ErrExecutorUnavailable, b.executor)
		}
		return exact.ImportSandboxOn(ctx, b.executor, b.spec, set, maxBytes, carried)
	}
	imp, ok := s.cfg.Runner.(runner.StateImporter)
	if !ok {
		return runner.Sandbox{}, runner.CarriedManifest{}, runner.ErrMoveCreate
	}
	return imp.ImportSandbox(ctx, b.spec, set, maxBytes, carried)
}
