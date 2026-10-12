// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TargetFacts is what ONE placement target says about itself: the classes it
// can enforce and the most one run may use. A pool's admission reads them per
// target, never the deployment's union, so a heterogeneous pool cannot lend one
// member's strength to another. A runner asserts them (runner_asserted).
type TargetFacts struct {
	Classes []types.ConfinementClass
	// CapacityAdvertised says the target states a per-run bound (a runner always
	// does; an executor of this deployment does not). When it is true a 0 is a
	// real bound of nothing, never "no bound".
	CapacityAdvertised         bool
	CPUMillisMax, MemoryMiBMax int64
	// EphemeralDiskEnforcement is what binds a run's disk_mib on this target (Capabilities.EphemeralDiskEnforcement
	// for one executor). Empty is unknown: a caller that refuses on `none` never refuses on unknown.
	EphemeralDiskEnforcement types.StorageEnforcement
	// KeepsEnded: the target keeps a stopped sandbox at a run's end (runner.SandboxEnder).
	KeepsEnded bool
	// UserDrives: the target can bind a member's drive (Capabilities.UserDrives).
	UserDrives bool
}

// TargetInspector answers per-target facts. The false answer means the facts are
// unknown (offline, nothing advertised yet, probe failed): a caller refuses on it
// and never reads it as zero or as everything.
type TargetInspector interface {
	RunnerFacts(runnerID string) (TargetFacts, bool)
	ExecutorFacts(ctx context.Context, executor string) (TargetFacts, bool)
}

// ExactExecutor creates the sandbox on the named configured executor and on no
// other: a run bound to a remote-provided pool goes where it was admitted, never
// to the first substrate that can enforce its class.
type ExactExecutor interface {
	CreateSandboxOn(ctx context.Context, executor string, spec SandboxSpec) (Sandbox, error)
}

// ErrExecutorUnavailable: the named executor is not one this deployment runs, or cannot enforce the
// run's class. A pinned placement is honoured or refused, never substituted.
var ErrExecutorUnavailable = errors.New("runner: the executor is not available")
