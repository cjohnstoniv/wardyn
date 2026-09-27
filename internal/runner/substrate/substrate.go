// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package substrate defines the confinement-substrate sub-interface: the seam
// beneath runner.Runner that lets a non-OCI microVM VMM (SmolVM, Firecracker,
// …) back a Confinement Class alongside the OCI/Docker substrate, without the
// control plane re-implementing the runner contract. The build-tag-free
// orchestrator (internal/runner/orchestrator) is the runner.Runner the control
// plane talks to; it multiplexes Substrates by Confinement Class and
// aggregates their capabilities.
//
// A Substrate brings up + tears down one governed sandbox (its per-run
// network, the wardyn-proxy sidecar, the agent unit). Every Substrate MUST:
//   - Prove confined egress, never merely assert it — EITHER L0 structural (no
//     default route; sole egress is the proxy sidecar) OR L1 network-policy
//     (a packet-filter default-denies egress except the proxy AND a boot-time
//     canary has positively confirmed enforcement on this host/cluster — a
//     policy object existing is not proof; see ClassSupport.NetworkPolicy).
//   - Advertise NO Confinement Classes if it can prove neither (fail closed,
//     never overclaim), except behind an explicit operator opt-out env var —
//     itself an admission of unconfined egress, not a third proof: the
//     substrate must still warn at construction/CreateSandbox naming what's
//     unconfined, AND keep advertising StructuralEgress=false,
//     NetworkPolicy=false, so an opted-out substrate never reads as confined
//     on /healthz.
//   - Error from CreateSandbox (never silently downgrade) before creating
//     anything, when the demanded Confinement Class can't be enforced.
//   - Never let the run token / secrets enter the agent's environment.
//   - Make teardown idempotent and reconstructable from the run id.
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
	// NetworkPolicy reports L1 (packet-filter default-deny except the proxy
	// sidecar), true only once a boot-time canary has positively confirmed
	// enforcement (see the package doc) — a policy object that exists but is
	// silently ignored by a non-enforcing CNI must never read true here.
	NetworkPolicy bool
	// NetworkPolicyAcknowledged (B1): an OPERATOR (not the canary) has accepted
	// an ambient-default-deny-shaped canary failure as expected
	// (WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY on k8s today). An acknowledgment,
	// never proof — mutually exclusive with NetworkPolicy=true and never a
	// substitute for it.
	NetworkPolicyAcknowledged bool
	// SessionRecording reports wardyn-rec PTY recording support.
	SessionRecording bool
	// UserDrives reports whether this substrate can BIND a member's user drive
	// (migration 0054) into the sandbox. False is the fail-closed default: the
	// control plane refuses a drive-carrying run rather than admit one this
	// substrate would reject at CreateSandbox. Aggregated as a CONJUNCTION (not
	// a union, unlike every OR-merged flag below) because it's consulted
	// BEFORE routing — one substrate unable to bind a drive caps the whole
	// deployment.
	UserDrives bool
	// ManagedFiles reports whether this substrate can deliver a root-owned
	// file (runner.SandboxSpec.ManagedFiles) in place before the agent's main
	// process runs. Same CONJUNCTION aggregation as UserDrives, same reason:
	// read before routing.
	ManagedFiles bool
	// EphemeralDiskEnforcement names WHAT ACTUALLY BINDS runner.Resources.DiskMiB
	// on this substrate — `filesystem` (a storage-driver quota refuses the
	// write), `eviction` (the kubelet kills the pod over the limit; never
	// refuses the write), or `none`/empty. Aggregated as the WEAKEST, not a
	// union: this is what the control plane tells an admin the number means,
	// so one substrate enforcing nothing caps the promise for the deployment.
	EphemeralDiskEnforcement types.StorageEnforcement
	// Freeze reports, PER CLASS, whether this substrate can pause/resume the
	// agent without losing state (the substrate-level analogue of
	// runner.Capabilities.Freeze). Copied straight through by the orchestrator
	// since only one substrate ever backs a given class; absent or false means
	// no freeze support for that class.
	Freeze map[types.ConfinementClass]bool
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
	// Exec launches the agent process inside ref, returning the
	// substrate-specific agent exec id ("" for exec-less/main-process
	// substrates) so the control plane can persist it for restart-safe
	// liveness.
	//
	// A substrate MAY support only ONE Exec per ref over its lifetime:
	// Kubernetes ephemeral containers are ADD-ONLY, so k8s can't honour a
	// second Exec the way docker's "latest Exec wins" re-exec does. Callers
	// MUST treat Exec as one-shot; a substrate that can't honour a second
	// Exec MUST error — never no-op, never return the PRIOR exec's id (a
	// stale agentExecID would misreport AgentStatus). EXEC-SPECIFIC: ExecStream
	// (below) MUST support repeated calls against the same ref.
	Exec(ctx context.Context, ref string, argv []string) (agentExecID string, err error)
	// Wait blocks until the agent process for ref exits and returns its code.
	Wait(ctx context.Context, ref string) (int, error)
	// Attach opens an interactive PTY session inside ref.
	Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error)
	// ExecStream launches spec.Argv inside ref as a fresh, streamable exec. See
	// runner.ExecStream's doc for the streaming/TTY-merge/invariant-3/4
	// contract. UNLIKE Exec, it MUST be repeatable against the SAME ref (one
	// long-lived sandbox, e.g. one call per SSH/SFTP channel); k8s implements
	// it on the streaming exec subresource, not an ephemeral container.
	ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error)
	// Status reports the sandbox lifecycle state.
	Status(ctx context.Context, ref string) (runner.Status, error)
	// AgentStatus reports the AGENT's state restart-safely given the persisted
	// agentExecID (inspects the exec for exec-based substrates; falls back to
	// Status when agentExecID is "").
	AgentStatus(ctx context.Context, ref, agentExecID string) (runner.Status, error)
	// StopSandbox is the graceful teardown (idempotent on a gone sandbox).
	StopSandbox(ctx context.Context, ref string) error
	// KillSandbox is the immediate kill-switch teardown (idempotent).
	KillSandbox(ctx context.Context, ref string) error
}
