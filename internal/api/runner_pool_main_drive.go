// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// poolMainDrive is what a pool's targets do with a run's main drive, as the pool list and both dry doors
// state it. The enforcement word is the WEAKEST across the targets and unknown whenever any target's is
// (offline, never reported) or there is no target, so a chip never claims more than every runner keeps.
// The basis is the runner's own assertion when any target is a person's own runner (or a user pool has
// no target at all); drives mount only when every target is a remote-hosted runner that mounts them.
func poolMainDrive(targets []poolTarget, scope types.RunnerPoolScope, grace time.Duration) *client.MainDriveFacts {
	f := &client.MainDriveFacts{Enforcement: client.MainDriveUnknown, Basis: types.RunEvidenceSubstrate}
	selfHosted := slices.ContainsFunc(targets, func(t poolTarget) bool { return t.runnerID != nil })
	if selfHosted || (len(targets) == 0 && scope == types.RunnerPoolUser) {
		f.Basis = types.RunEvidenceRunnerAsserted
	}
	known := len(targets) > 0
	keeps, mounts := known, known && !selfHosted
	weakest := types.StorageEnforcementFilesystem
	for _, t := range targets {
		known = known && !t.offline && t.facts.EphemeralDiskEnforcement != ""
		keeps = keeps && !t.offline && t.facts.KeepsEnded
		mounts = mounts && !t.offline && t.facts.UserDrives
		if diskEnforcementRank(t.facts.EphemeralDiskEnforcement) < diskEnforcementRank(weakest) {
			weakest = t.facts.EphemeralDiskEnforcement
		}
	}
	if known {
		f.Enforcement, f.Coverage = mainDriveWord(weakest)
	}
	f.DriveMounts = mounts
	if keeps && grace > 0 {
		f.Keep = client.MainDriveKeep{Supported: true, GraceSec: int(grace.Seconds())}
	}
	return f
}

// mainDriveWord is the client word for a target's enforcement and what it covers. Anything but the two
// real caps (a scheduling request, something outside Wardyn) claims nothing.
func mainDriveWord(e types.StorageEnforcement) (client.MainDriveEnforcement, client.MainDriveCoverage) {
	switch e {
	case types.StorageEnforcementFilesystem:
		return client.MainDriveFilesystem, client.MainDriveCoverageAll
	case types.StorageEnforcementEviction:
		return client.MainDriveEviction, client.MainDriveCoverageScratch
	}
	return client.MainDriveNone, ""
}

// poolMainDriveFor is the facts for one permitted pool the way the list and the dry doors both ask, over
// every kind of runner in it: the remote-hosted runners this deployment runs (each read through read)
// and, in a user pool, the caller's own runners the availability check already resolved (own).
func (s *Server) poolMainDriveFor(row store.RunnerPoolCatalogueRow, own []poolTarget, read func(executor string) (runner.TargetFacts, bool)) *client.MainDriveFacts {
	remote, _ := executorTargets(row, s.configuredExecutors(), read)
	return poolMainDrive(append(slices.Clone(own), remote...), row.Pool.Scope, s.cfg.EndedRunGrace)
}

// onceExecutorFacts reads each executor's facts at most once per call site, so a failing executor held
// by several pools is asked once, not once per pool.
func (s *Server) onceExecutorFacts(ctx context.Context) func(executor string) (runner.TargetFacts, bool) {
	type read struct {
		facts runner.TargetFacts
		known bool
	}
	seen := map[string]read{}
	return func(e string) (runner.TargetFacts, bool) {
		r, ok := seen[e]
		if !ok {
			r.facts, r.known = s.executorFacts(ctx, e)
			seen[e] = r
		}
		return r.facts, r.known
	}
}
