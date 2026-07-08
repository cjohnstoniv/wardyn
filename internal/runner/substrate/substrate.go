// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package substrate defines the confinement-substrate sub-interface: the seam
// beneath the runner.Runner surface that lets a non-OCI microVM VMM (SmolVM,
// Firecracker, …) back a Confinement Class alongside the OCI/Docker substrate,
// without the control plane (or each substrate) re-implementing the runner
// contract. The build-tag-free orchestrator (internal/runner/orchestrator) is
// the runner.Runner the control plane talks to; it multiplexes Substrates by
// Confinement Class and aggregates their capabilities.
//
// A Substrate owns the mechanism of bringing up + tearing down a governed
// sandbox (its isolated, gatewayless per-run network, the wardyn-proxy sidecar,
// and the agent unit) on one substrate. Every Substrate MUST uphold Wardyn's
// non-negotiables for the sandboxes it creates:
//   - L0 structural egress: the agent has NO default route; its SOLE egress path
//     is the wardyn-proxy sidecar (no direct off-host route).
//   - Fail closed: CreateSandbox MUST error (never silently downgrade) when the
//     demanded Confinement Class cannot be enforced, before creating anything.
//   - The run token / secrets NEVER enter the agent's environment.
//   - Teardown is idempotent and reconstructable from the run id (crash-safe).
package substrate

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ClassSupport reports the Confinement Classes a substrate can enforce on this
// host and the concrete substrate label backing each (e.g. CC3 -> "oci/kata-qemu").
// It is the substrate-level analogue of runner.Capabilities; the orchestrator
// aggregates these across substrates into the runner.Capabilities it advertises.
type ClassSupport struct {
	// Classes the substrate can enforce, strongest last. A class is listed ONLY
	// when its enforcing runtime is actually available (never overclaim).
	Classes []types.ConfinementClass
	// Resolved maps each available class to its substrate label ("oci/<runtime>").
	Resolved map[types.ConfinementClass]string
	// StructuralEgress reports L0 (no default route; sole egress = wardyn-proxy).
	StructuralEgress bool
	// SessionRecording reports wardyn-rec PTY recording support.
	SessionRecording bool
}

// Substrate is runner.Runner's lifecycle contract for ONE confinement substrate,
// with Capabilities replaced by Classes (per-class substrate detail). The OCI
// substrate (internal/runner/docker) satisfies it today; a non-OCI VMM satisfies
// the same contract to plug into CC3.
type Substrate interface {
	// Name reports the substrate kind ("docker"/OCI today), surfaced on /healthz.
	Name() string
	// Classes reports the enforceable Confinement Classes + their substrate labels.
	Classes(ctx context.Context) (ClassSupport, error)
	// CreateSandbox provisions the run's isolated network + proxy + agent unit,
	// fail-closed with full rollback on any error.
	CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error)
	// Exec launches the agent process inside the sandbox ref.
	Exec(ctx context.Context, ref string, argv []string) error
	// Wait blocks until the agent process for ref exits and returns its code.
	Wait(ctx context.Context, ref string) (int, error)
	// Attach opens an interactive PTY session inside ref.
	Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error)
	// Status reports the sandbox lifecycle state.
	Status(ctx context.Context, ref string) (runner.Status, error)
	// StopSandbox is the graceful teardown (idempotent on a gone sandbox).
	StopSandbox(ctx context.Context, ref string) error
	// KillSandbox is the immediate kill-switch teardown (idempotent).
	KillSandbox(ctx context.Context, ref string) error
}
