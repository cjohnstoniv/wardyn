// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runner defines the target-agnostic sandbox lifecycle contract.
// The control plane ONLY talks to this interface — it must contain zero
// Docker- or Kubernetes-specific code. Drivers live in subpackages
// (currently runner/docker) and are conformance-tested identically.
package runner

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Capabilities declares what a driver (on this host/cluster) can actually
// enforce. The control plane uses this to honor Confinement Class policy:
// it must refuse to schedule a run whose policy demands more than the
// driver declares. Never claim a control that is not structurally enforced.
type Capabilities struct {
	Driver string `json:"driver"` // e.g. "docker"
	// ConfinementClasses available on this host/cluster, strongest last.
	ConfinementClasses []types.ConfinementClass `json:"confinement_classes"`
	// Resolved maps each available ConfinementClass to the concrete substrate
	// label that enforces it (e.g. "oci/runc", "oci/runsc", "oci/kata-qemu"), so
	// /healthz can advertise WHICH runtime backs each class — the seam that makes
	// CC3 substrate-pluggability visible to operators. Nil when a driver does not
	// report substrate detail.
	Resolved map[types.ConfinementClass]string `json:"confinement_substrates,omitempty"`
	// StructuralEgress reports L0 support: sandbox has no default route and
	// its only egress path is the wardyn-proxy sidecar.
	StructuralEgress bool `json:"structural_egress"`
	// NetworkPolicy reports L1 support (nftables / NetworkPolicy default-deny).
	NetworkPolicy bool `json:"network_policy"`
	// WarmPools reports pre-provisioned sandbox support.
	WarmPools bool `json:"warm_pools"`
	// SessionRecording reports wardyn-rec sidecar support.
	SessionRecording bool `json:"session_recording"`
}

// SandboxSpec is everything a driver needs to create one governed sandbox.
type SandboxSpec struct {
	RunID            uuid.UUID
	Image            string // resolved agent/workspace OCI image
	ConfinementClass types.ConfinementClass
	// Env is non-secret environment. Secrets NEVER pass through here —
	// they are injected proxy-side or resolved late via the broker.
	Env map[string]string
	// ProxyConfig wires the L0 path: the sandbox's only egress is the
	// wardyn-proxy sidecar identified here.
	ProxyConfig ProxyConfig
	// Resources are hard caps (cgroups / ResourceQuota).
	Resources Resources
	// Labels are attached to the sandbox for attestation selectors and audit.
	Labels map[string]string
	// Mounts are operator/policy-controlled host bind mounts into the sandbox
	// (e.g. a host repo at ~/work for the WSL-migration substrate / a persistent
	// workspace). SECURITY: these are POLICY-controlled, NEVER attacker-controlled.
	// The ONLY population path is internal/api dispatch copying a policy's
	// RunPolicySpec.WorkspaceMounts here; the create-run HTTP request body has no
	// mounts field, so a prompt-injected agent or a malicious run requester can
	// never choose a host mount. Drivers apply these as bind mounts AND enforce a
	// deny-list defense-in-depth (see runner/docker/driver.go) even though the
	// values came from policy. Default ReadOnly.
	Mounts []Mount
	// Interactive marks a run that comes up idle for `wardyn attach` (no task is
	// exec'd). Drivers use it to prepare the workspace on the idle main process —
	// e.g. clone the repo into ~/work — so the attach shell isn't empty. A non-
	// interactive run ignores it (its task exec does the preparation).
	Interactive bool
}

// Mount is one operator/policy-controlled host bind mount into the sandbox.
// Source is a host path; Target is the in-container path (drivers restrict it
// to an allowed prefix, e.g. under /home/agent or /work). ReadOnly defaults to
// true (RW only when the policy explicitly opts in). See SandboxSpec.Mounts for
// the security model: mounts are operator/policy-controlled, never request-set.
type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type ProxyConfig struct {
	// RunToken authenticates the sidecars to the control plane (identity
	// provider verifies; it is NOT a secret usable outside the platform).
	RunToken string
	// ControlPlaneURL is where sidecars stream decisions/recordings.
	ControlPlaneURL string
	// Policy is the run's egress policy, handed verbatim to the wardyn-proxy
	// sidecar (default-deny domain allowlist, method rules, first-use flag).
	// Drivers MUST deliver it to the sidecar at launch: a proxy without a
	// policy fails closed and the sandbox has no working egress at all.
	Policy types.RunPolicySpec
	// Injection lists the run's auto-mintable api_key grants the proxy
	// resolves at startup (secret values live only in proxy memory — never in
	// the sandbox). Approval-gated api_key grants are NOT included: they would
	// block proxy startup, which fails closed if any injection mint fails.
	Injection []InjectionGrant
	// MITMCACertPEM / MITMCAKeyPEM are the OPTIONAL per-run TLS-MITM CA (PEM)
	// delivered to the proxy sidecar when the policy opts into intercept_tls. The
	// CA private key reaches ONLY the proxy (never the sandbox); the sandbox
	// trusts the public cert (delivered separately via the agent env). Empty =>
	// opaque CONNECT passthrough (no MITM).
	MITMCACertPEM string
	MITMCAKeyPEM  string
	// MITMHosts are OPERATOR-CONFIGURED corp artifact hosts the proxy may TLS-MITM
	// in addition to the built-in LLM hosts, so a corporate registry token can be
	// injected on the wire (the sandbox never holds it). A tight per-host operator
	// allowlist sourced from site-config's artifact overrides — NEVER a blanket and
	// NEVER attacker-controlled (dispatch populates it, the sandbox cannot).
	MITMHosts []string
	// MITMLLM reports whether TLS-MITM of the built-in LLM hosts (Anthropic/OpenAI)
	// is intended for this run (subscription injection or intercept_tls) — as opposed
	// to a CA minted only for artifact-token injection. See proxy.Config.MITMLLM.
	MITMLLM bool
	// UpstreamProxyURL is the OPTIONAL corporate parent proxy the sidecar chains
	// egress through (http://[user:pass@]host[:port] — https-to-proxy is rejected
	// by the sidecar's own config validation, parseUpstreamProxy). Threaded
	// verbatim to the proxy sidecar; control-plane calls bypass it. Empty =>
	// direct dial.
	//
	// Sourced operator-wide at dispatch (internal/api/runs.go dispatchWithVerify)
	// from the persisted site-config's UpstreamProxySecretRef, resolved to the
	// secret's value. Any resolution failure (unset ref, missing secret, non-http
	// URL) leaves this "" rather than failing the run — see
	// resolveUpstreamProxyURL and its audit event run.upstream_proxy.resolve.
	UpstreamProxyURL string
}

// InjectionGrant pairs an api_key credential grant with its proxy-side
// injection rule (host/header/format/secret name — never the secret value).
type InjectionGrant struct {
	GrantID uuid.UUID            `json:"grant_id"`
	Rule    egress.InjectionRule `json:"rule"`
}

// Resources are the hard sandbox caps the driver applies as cgroup / storage
// limits. A ZERO field means "use the driver's conservative platform default"
// (the docker driver fills CPU/memory/PIDs unconditionally so EVERY sandbox is
// capped even when policy sets nothing). The control plane copies these from a
// policy's types.ResourceLimits at dispatch.
type Resources struct {
	CPUMillis int64
	MemoryMiB int64
	// PidsLimit caps the number of processes/threads in the sandbox — the
	// fork-bomb guard for the host PID space. Zero => driver default.
	PidsLimit int64
	// DiskMiB caps writable storage. Best-effort: the docker driver applies it
	// only when the daemon storage driver supports a per-container quota
	// (overlay2 with project quota, or btrfs/zfs); otherwise it warns and runs
	// uncapped rather than hard-failing the run.
	DiskMiB int64
}

// AttachOptions configures an interactive attach. Cols/Rows are the initial PTY
// window size; zero values let the driver pick a sane default (e.g. 80x24).
type AttachOptions struct {
	Cols uint16
	Rows uint16
}

// Session is a live, bidirectional interactive PTY stream into a RUNNING
// sandbox, opened by Runner.Attach. It is the human-facing analogue of the
// agent exec: a person types into Write and reads the terminal back from Read.
//
// SECURITY (invariant 3): the interactive shell runs INSIDE the existing
// sandbox, so it is bounded by exactly the same L0 structural-egress and
// confinement envelope as the agent process. Attach opens NO new network path —
// the stream flows control-plane -> dockerd -> container, never through the
// sandbox's HTTP_PROXY egress path, and egress/mint enforcement stays at the
// proxy/broker. Attach therefore grants a terminal, not a new egress route.
//
// A Session is NOT safe for concurrent Read/Write from multiple goroutines on
// the same direction, but the typical pump runs Read in one goroutine and Write
// in another, which is supported.
type Session interface {
	// Read copies terminal output (PTY bytes) into p. It returns io.EOF when the
	// shell exits or the stream is closed.
	Read(p []byte) (int, error)
	// Write sends keystrokes (PTY bytes) into the shell.
	Write(p []byte) (int, error)
	// Resize informs the PTY of a new window size (e.g. on a browser resize).
	Resize(ctx context.Context, cols, rows uint16) error
	// Close tears down ONLY the interactive exec stream. It does NOT stop the
	// sandbox, the agent process, or any sidecar — detaching a human leaves the
	// run exactly as it was.
	Close() error
}

// Sandbox is a handle to a created sandbox.
type Sandbox struct {
	Ref    string // container ID / pod name
	Driver string
	// EnforcedClass is what the driver actually applied (>= requested or error).
	EnforcedClass types.ConfinementClass
}

// Status reports observed sandbox state.
type Status struct {
	State    types.RunState
	ExitCode *int
	Message  string
}

// Runner is the lifecycle contract. Implementations must be safe for
// concurrent use. Every method must be idempotent where the verb implies it
// (Stop/Kill on a gone sandbox return nil).
type Runner interface {
	Name() string
	Capabilities(ctx context.Context) (Capabilities, error)
	// CreateSandbox provisions the sandbox AND its sidecars (proxy, recorder)
	// with L0 confinement: no default route, egress only via the proxy.
	CreateSandbox(ctx context.Context, spec SandboxSpec) (Sandbox, error)
	// Exec starts the agent process inside the sandbox (PTY attached when
	// recording). Returns when the process has been started, not finished. The
	// returned agentExecID identifies the started process for exec-based substrates
	// (the docker idle-container + `docker exec` path); it is "" for exec-less /
	// main-process substrates (krun), where the container IS the agent. Persist it
	// so the crash reconciler can observe agent liveness across a wardynd restart
	// via AgentStatus (U008/U039).
	Exec(ctx context.Context, ref string, argv []string) (agentExecID string, err error)
	// Wait blocks until the agent process started by Exec for this sandbox ref
	// has exited, returning its exit code. It is ONLY valid after a successful
	// Exec on the same ref (it observes the agent exec Exec created). Wait
	// honours ctx cancellation/deadline and returns an error if no agent exec
	// is tracked for ref (e.g. Exec was never called, or the ref is unknown).
	Wait(ctx context.Context, ref string) (exitCode int, err error)
	// Attach opens a NEW interactive exec (an interactive shell) inside the
	// already-RUNNING sandbox ref and returns a live PTY Session. This is the
	// foundation of interactive session mode: a human attaches to a live PTY in
	// a running sandbox. The exec is SEPARATE from the agent process Wait tracks
	// — it is a fresh shell, so attaching/detaching never affects the agent.
	//
	// Session.Close tears down ONLY the exec stream, NOT the sandbox: detaching
	// leaves the run and its sidecars exactly as they were. The interactive
	// shell is bounded by the SAME L0 egress + confinement envelope as the agent
	// (it runs inside the existing sandbox); Attach opens no new network path
	// (invariant 3). Callers MUST record the human principal for attribution
	// (invariant 4) at the call site (the runner is identity-agnostic).
	Attach(ctx context.Context, ref string, opts AttachOptions) (Session, error)
	Status(ctx context.Context, ref string) (Status, error)
	// AgentStatus reports the AGENT's observed state in a restart-safe way, given
	// the agentExecID Exec returned (persisted on the run row). For exec-based
	// substrates it inspects that exec, so a run whose agent has exited reports a
	// terminal State + ExitCode even while the idle container is still up — the
	// distinction container-level Status cannot make after a restart lost the
	// in-memory exec map. When agentExecID is "" (exec-less/main-process, or Exec
	// never ran) it falls back to Status, where the container IS the agent.
	AgentStatus(ctx context.Context, ref, agentExecID string) (Status, error)
	// StopSandbox is the graceful path (lifecycle auto-stop).
	StopSandbox(ctx context.Context, ref string) error
	// KillSandbox is the kill-switch path: immediate teardown. The control
	// plane cascades identity + credential revocation around this call.
	KillSandbox(ctx context.Context, ref string) error
}
