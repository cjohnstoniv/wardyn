// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package substrate defines the confinement-substrate sub-interface: the seam
// beneath runner.Runner that lets a non-OCI microVM VMM back a Confinement
// Class alongside the OCI/Docker substrate.
//
// A Substrate brings up + tears down one governed sandbox. Every Substrate
// MUST: prove confined egress rather than merely assert it (L0 structural or
// L1 network-policy, see ClassSupport.NetworkPolicy); advertise NO
// Confinement Classes if it can prove neither, except behind an explicit,
// still-warning operator opt-out; error from CreateSandbox, never silently
// downgrade, when the demanded class can't be enforced; never let the run
// token/secrets reach the agent's environment; make teardown idempotent and
// reconstructable from the run id.
package substrate

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ClassSupport reports the Confinement Classes a substrate can enforce and
// the substrate label backing each.
type ClassSupport struct {
	Classes                   []types.ConfinementClass          // strongest last; never overclaim
	Resolved                  map[types.ConfinementClass]string // substrate label ("oci/<runtime>") per class
	StructuralEgress          bool                              // L0: no default route; sole egress = wardyn-proxy
	NetworkPolicy             bool                              // L1, true only once a boot-time canary confirms enforcement
	NetworkPolicyAcknowledged bool                              // an OPERATOR accepted a canary failure; never a substitute for NetworkPolicy=true
	SessionRecording          bool                              // wardyn-rec PTY recording support
	// UserDrives and ManagedFiles are CONJUNCTION-aggregated (read BEFORE
	// routing, unlike every OR-merged flag above): can this substrate BIND a
	// member's user drive / deliver a root-owned file; false fail-closed.
	UserDrives   bool
	ManagedFiles bool
	// EphemeralDiskEnforcement names what binds runner.Resources.DiskMiB —
	// `filesystem`, `eviction`, or `none`/empty. Aggregated as the WEAKEST.
	EphemeralDiskEnforcement types.StorageEnforcement
	Freeze                   map[types.ConfinementClass]bool // per-class pause/resume support; absent/false = none
}

// Substrate is runner.Runner's lifecycle contract for ONE confinement substrate.
type Substrate interface {
	// Name reports the substrate kind, surfaced on /healthz.
	Name() string
	// Classes reports the enforceable Confinement Classes + their substrate labels.
	Classes(ctx context.Context) (ClassSupport, error)
	// CreateSandbox provisions the run's sandbox, fail-closed with full rollback on any error.
	CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error)
	// Exec launches the agent process inside ref, returning the exec id for
	// restart-safe liveness. A substrate MAY support only ONE Exec per ref
	// (k8s ephemeral containers are ADD-ONLY) and MUST error rather than
	// no-op or return the PRIOR id when it can't honour a second.
	Exec(ctx context.Context, ref string, argv []string) (agentExecID string, err error)
	// Wait blocks until the agent process for ref exits and returns its code.
	Wait(ctx context.Context, ref string) (int, error)
	// Attach opens an interactive PTY session inside ref.
	Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error)
	// ExecStream launches a fresh, streamable exec, repeatable against the SAME ref (see runner.ExecStream's doc).
	ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error)
	// Status reports the sandbox lifecycle state.
	Status(ctx context.Context, ref string) (runner.Status, error)
	// AgentStatus reports the AGENT's state restart-safely, falling back to Status when agentExecID is "".
	AgentStatus(ctx context.Context, ref, agentExecID string) (runner.Status, error)
	// StopSandbox is the graceful teardown (idempotent on a gone sandbox).
	StopSandbox(ctx context.Context, ref string) error
	// KillSandbox is the immediate kill-switch teardown (idempotent).
	KillSandbox(ctx context.Context, ref string) error
}
