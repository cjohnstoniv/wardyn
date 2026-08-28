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
// sandbox (its isolated per-run network, the wardyn-proxy sidecar, and the
// agent unit) on one substrate. Every Substrate MUST uphold Wardyn's
// non-negotiables for the sandboxes it creates:
//   - Confined egress, proven EITHER of two ways — never merely asserted:
//   - L0 structural: the agent has NO default route; its SOLE egress path is
//     the wardyn-proxy sidecar (no direct off-host route). This is the
//     docker substrate's guarantee: absence of route, not a filter to bypass.
//   - L1 network-policy: a packet-filter layer (e.g. Kubernetes NetworkPolicy)
//     default-denies the agent's egress except the proxy sidecar, AND the
//     substrate has PROVEN that enforcement on THIS host/cluster — a policy
//     object existing is not proof; a boot-time canary that positively
//     confirms the deny takes effect (and refuses to boot when it does not)
//     is. See ClassSupport.NetworkPolicy.
//   - A substrate that can prove NEITHER MUST advertise no Confinement Classes
//     at all (fail closed, never overclaim) — the sole documented exception is
//     an explicit, operator-set opt-out env var read by that substrate. That
//     opt-out is itself an admission of unconfined egress, not a third proof,
//     and it MUST NOT be an invisible downgrade: the substrate (a) emits a
//     warning at construction or CreateSandbox naming what is going
//     unconfined, and (b) still advertises StructuralEgress=false AND
//     NetworkPolicy=false regardless — an opted-out substrate must never read
//     identical to a genuinely confined one on /healthz.
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
	// NetworkPolicy reports L1 (a packet-filter default-deny, e.g. Kubernetes
	// NetworkPolicy, default-denying the agent's egress except the proxy
	// sidecar) — PROVEN on this host/cluster, not merely configured. A
	// substrate sets this true only after a boot-time canary has positively
	// confirmed the deny is actually enforced (see the package doc); a
	// NetworkPolicy object that exists but is silently ignored by a
	// non-enforcing CNI is exactly the false claim this field must never make.
	// StructuralEgress and NetworkPolicy are not mutually exclusive in
	// principle, but today's substrates each prove exactly one.
	NetworkPolicy bool
	// NetworkPolicyAcknowledged (B1) is true when an OPERATOR has accepted an
	// ambient-default-deny-shaped canary failure as expected rather than the
	// canary proving enforcement (WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY on the
	// k8s substrate today). An acknowledgment, never proof — mutually
	// exclusive with NetworkPolicy=true (alternate outcomes of the same
	// boot-time canary), and must never be treated as satisfying an
	// enforcement requirement NetworkPolicy alone gates.
	NetworkPolicyAcknowledged bool
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
	// Exec launches the agent process inside the sandbox ref, returning the
	// substrate-specific agent exec id ("" for exec-less/main-process substrates)
	// so the control plane can persist it for restart-safe liveness.
	//
	// A substrate MAY support only ONE Exec per ref over the sandbox's
	// lifetime: Kubernetes ephemeral containers are ADD-ONLY (a pod's
	// ephemeral-container list can only grow, never be replaced), so a k8s
	// substrate cannot honour a second Exec on the same ref the way the
	// docker substrate's "latest Exec wins" re-exec does today. Callers MUST
	// NOT re-Exec a ref expecting replace semantics — treat Exec as
	// one-shot per sandbox. A substrate that cannot honour a second Exec on
	// ref MUST return an error: never silently no-op, and never return the
	// PRIOR exec's id — a stale agentExecID would point AgentStatus at the
	// wrong process (fail closed; don't misreport liveness).
	//
	// This one-shot constraint is EXEC-SPECIFIC. ExecStream (below) is the
	// opposite: a substrate MUST support repeated ExecStream calls against
	// the same ref.
	Exec(ctx context.Context, ref string, argv []string) (agentExecID string, err error)
	// Wait blocks until the agent process for ref exits and returns its code.
	Wait(ctx context.Context, ref string) (int, error)
	// Attach opens an interactive PTY session inside ref.
	Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error)
	// ExecStream launches spec.Argv inside ref as a fresh, streamable exec.
	// See runner.ExecStream's doc for the streaming, TTY-merge, and
	// invariant-3/4 security contract.
	//
	// UNLIKE Exec, ExecStream MUST be repeatable against the SAME ref: a
	// substrate MUST support many ExecStream calls against one long-lived
	// sandbox (e.g. one per SSH/SFTP channel). A k8s substrate implements
	// ExecStream on the streaming exec subresource (pods/<name>/exec —
	// repeatable, leaves no pod-spec residue), NOT an ephemeral container
	// (add-only — that limit is what makes Exec one-shot, not ExecStream).
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
