// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runner defines the target-agnostic sandbox lifecycle contract.
// The control plane ONLY talks to this interface — it must contain zero
// Docker- or Kubernetes-specific code. Drivers live in subpackages
// (currently runner/docker) and are conformance-tested identically.
package runner

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
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
	// NetworkPolicyAcknowledged (B1): an operator has accepted an
	// ambient-default-deny-shaped canary failure as expected rather than
	// proven — see substrate.ClassSupport.NetworkPolicyAcknowledged's doc.
	// omitempty: absent on every driver that predates B1 reads the same as
	// false.
	NetworkPolicyAcknowledged bool `json:"network_policy_acknowledged,omitempty"`
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
	// MemberMountRoots, when non-nil, marks this run as one whose mounts were
	// authored by a MEMBER (a member-owned workspace's local_dir) and carries
	// the operator/MDM-set roots those mounts must resolve inside. Resolved at
	// create-run from the owning member's principal (MemberMountPolicy.RootsFor)
	// and re-checked by the driver at BIND time — the last moment this process
	// can resolve the real path — via ValidateMemberMountSource.
	//
	// NIL for every operator/non-member run, and nil means the driver does
	// EXACTLY what it does today: the member gate is purely additive and can
	// never narrow an operator mount. See member_mount.go for the threat model.
	MemberMountRoots []string
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
	// MemberAuthored marks a bind whose SOURCE a MEMBER chose — a member-owned
	// workspace's local_dir. ONLY these are re-checked against
	// SandboxSpec.MemberMountRoots at bind time, because the roots bound what a
	// MEMBER may name and nothing else: the same spec also carries binds WARDYN
	// ITSELF authored (the subscription ~/.claude credential staging, the Bedrock
	// ~/.aws dir) and an operator-owned workspace's dirs, none of which live
	// under any member root. Checking those too refused the credential mounts
	// EVERY model run needs, so no member-owned workspace could run at all on a
	// subscription or Bedrock deployment.
	//
	// Set by internal/api dispatch from the run's member-owned workspaces
	// (memberMountPosture); false — the operator default — everywhere else.
	MemberAuthored bool `json:"member_authored,omitempty"`
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
	// GitGrants is the git-broker per-repo allowlist ("<org>/<repo>" -> github_token
	// grant id) delivered to the proxy sidecar's /wardyn/gh/ route so the sandbox
	// reaches only its granted repos (never all of github.com) and the token stays
	// proxy-side. Populated at dispatch from the run's github grants; empty => no
	// repo brokered. See proxy.Config.GitGrants.
	GitGrants map[string]uuid.UUID
	// PATGrants is the git_pat broker's per-HOST allowlist ("host" -> the grant
	// to mint from), backing the proxy's /wardyn/git/ route so a non-GitHub
	// forge's PAT is minted proxy-side and never enters the sandbox. Empty => no
	// host brokered. See proxy.Config.PATGrants.
	PATGrants map[string]proxy.PATGrant
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
	// TrustedCAPEM is the operator's corporate CA bundle (WARDYN_TRUSTED_CA_FILE,
	// api.Config.TrustedCAPEM), forwarded verbatim so the sidecar's own outbound
	// TLS additionally trusts it. Threaded to the proxy via proxy.Config's
	// identically-named field (WARDYN_PROXY_CONFIG_JSON, BuildProxyConfig below).
	// Control-plane-authored, same trust boundary as MITMCACertPEM/MITMCAKeyPEM
	// above; empty => system roots only, byte-identical to today.
	TrustedCAPEM string
	// InternalHosts are the operator-declared internal hostnames
	// (SiteConfig.InternalHosts, forwarded verbatim) eligible for the proxy's
	// private-IP-guard lift. Control-plane-authored (the sandbox cannot set
	// this); empty => no lift, byte-identical to today. Threaded to the proxy
	// via proxy.Config's identically-named field (BuildProxyConfig below).
	InternalHosts []types.InternalHost
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

// ErrExecStreamUnsupported is the sentinel a Runner/Substrate returns from
// ExecStream when it has no implementation for the primitive (a stub, a fake,
// or a driver that has not wired it up yet). Callers — notably the
// conformance suite's testExecStream — use errors.Is against this exact
// sentinel to skip cleanly on "not implemented" while still FAILING on any
// other error, so an implemented-but-broken ExecStream cannot skip green by
// returning some other error.
var ErrExecStreamUnsupported = errors.New("runner: ExecStream not supported")

// ExecSpec describes one exec launched via Runner.ExecStream: the argv to run,
// exec-scoped environment, and whether it runs under a PTY.
type ExecSpec struct {
	Argv []string
	// Env is additional exec-scoped environment (on top of the sandbox's own).
	Env []string
	// TTY requests a pseudo-terminal. PTY semantics MERGE stdout and stderr
	// onto ExecSession.Stdout (ExecSession.Stderr then reads io.EOF
	// immediately — there is no separate channel to read). Without a TTY,
	// stdout and stderr are delivered as SEPARATE streams: a binary protocol
	// riding stdout (SFTP, socat) would be corrupted by interleaved stderr
	// bytes, so a merged stream is only acceptable under PTY semantics.
	TTY bool
	// Cols, Rows seed the initial PTY window size (TTY only); zero lets the
	// implementation pick a default.
	Cols, Rows uint16
}

// ExecSession is a live, bidirectional stream into ONE exec started by
// Runner.ExecStream — the streaming-primitive analogue of Session (which is
// shaped for an interactive attach shell). It is a PLAIN STRUCT of
// streams/closures, deliberately NEVER an interface: ExecStream has exactly
// one production implementation per substrate plus test fakes, and a struct
// lets a fake construct a partial ExecSession (a canned Wait, a
// strings.Reader for Stdout, a nil Stdin) by filling in fields directly —
// there is no method set to satisfy, so no fake can ever "implement this
// wrong."
//
// Resize, Wait, and Close take NO context parameter: an implementation binds
// them to whatever context it created the exec with. Callers that need a
// fresh deadline per operation should scope the ctx passed to ExecStream
// itself accordingly.
type ExecSession struct {
	// Stdin writes to the exec's standard input. Close HALF-closes the write
	// side only (the exec observes EOF on stdin) — implementations MUST NOT
	// tear down Stdout/Stderr when Stdin.Close is called; those streams may
	// still be flowing.
	Stdin io.WriteCloser
	// Stdout carries standard output. With ExecSpec.TTY=true it ALSO carries
	// stderr (PTY semantics merge the two onto one stream); with TTY=false it
	// carries ONLY stdout — see Stderr.
	Stdout io.Reader
	// Stderr carries standard error as a SEPARATE stream when ExecSpec.TTY is
	// false. When TTY is true there is no separate stderr channel to read
	// (PTY semantics already merged it onto Stdout), so Stderr reads io.EOF
	// immediately.
	//
	// STREAMING CONTRACT (TTY=false): Stdout and Stderr are UNBUFFERED
	// io.Pipes fed by a single background demux goroutine off ONE underlying
	// connection: a single undrained stderr byte blocks the demux goroutine,
	// Stdout, AND Wait (which observes the same exec). Callers MUST start
	// draining Stderr BEFORE (or concurrently with) the first Stdout read.
	// This mirrors how every demultiplexed docker/moby attach stream must be
	// consumed; it is not implementation-specific.
	Stderr io.Reader
	// Resize changes the PTY window size. A no-op returning nil when the exec
	// has no TTY.
	Resize func(cols, rows uint16) error
	// Wait blocks until the exec exits and returns its exit code.
	Wait func() (int, error)
	// Close tears down ONLY this exec stream — never the sandbox, the agent
	// process, or any sidecar.
	Close func() error
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

// ErrExecNeverStarted is the sentinel a Runner's Wait returns when it can
// prove the agent exec will NEVER reach a terminal state on its own — e.g.
// the k8s driver's ephemeral "wardyn-agent" container stuck Waiting on a
// hard-failure Reason (ImagePullBackOff, CreateContainerConfigError, ...)
// that will not resolve without intervention. Distinct from every other Wait
// error (a transient probe error, ctx cancellation): those mean "the agent
// might still be running, we just couldn't observe it right now" and the
// caller retries/hands off to the reconciler; this one means "the task never
// ran and never will", so the caller (startCompletionWatcher,
// runs_lifecycle.go) must fail the run immediately rather than treat it as a
// transient hiccup to hand off.
var ErrExecNeverStarted = errors.New("runner: agent exec never started")

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
	// via AgentStatus.
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
	// ExecStream launches spec.Argv inside the already-RUNNING sandbox ref as a
	// fresh, streamable exec, distinct from both the agent process Exec starts
	// (Wait/AgentStatus observe THAT one, not this) and from Attach's
	// interactive shell (no PTY tmux session, no shell wrapping — argv runs
	// directly). See ExecSpec/ExecSession for the streaming and TTY-merge
	// contract.
	//
	// UNLIKE Exec, ExecStream MUST be repeatable against the SAME ref: a
	// long-lived consumer (an SSH/SFTP bridge, a multiplexed shell) opens ONE
	// ExecStream per channel against ONE long-lived sandbox ref for the life
	// of the connection, so a substrate that errored or no-op'd on a second
	// call would break that consumer outright. A k8s substrate implements
	// ExecStream on the streaming exec subresource (pods/<name>/exec —
	// repeatable, leaves no pod-spec residue), NOT an ephemeral container
	// (add-only; that add-only limit is what makes Exec one-shot, not
	// ExecStream — see Substrate.Exec's doc).
	//
	// SECURITY: ExecStream opens no new network path (invariant 3). Callers
	// MUST record the human principal for attribution (invariant 4) at the
	// call site (the runner is identity-agnostic) — a raw argv makes this the
	// MORE dangerous of the two streaming primitives, not less.
	ExecStream(ctx context.Context, ref string, spec ExecSpec) (*ExecSession, error)
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

// ImageChecker is an OPTIONAL Runner capability (W20-W20-record-image-5): a
// substrate whose local image cache can go stale out from under a workspace's
// cached image_ref (the docker driver — a pruned/removed local image; the
// daemon that built it is gone) implements this so a stale cache can be
// detected and fallen through to a rebuild, instead of the cached ref being
// trusted forever and every launch failing at "no such image" until an
// operator finds and clears the row by hand. A substrate that pulls fresh per
// launch (k8s: the kubelet pulls, there is no local cache to go stale) has
// nothing to report and simply doesn't implement this — callers type-assert
// and treat "doesn't implement" the same as a check error: unknown, so trust
// the cache (fail-open, the same posture resolveWorkspaceImage already takes
// everywhere else).
type ImageChecker interface {
	// ImagePresent reports whether ref is present in the substrate's local
	// image store right now.
	ImagePresent(ctx context.Context, ref string) (bool, error)
}

// ImageRemover is an OPTIONAL Runner capability (bug-workspace-1): a substrate
// with a local image store (the docker driver) implements this so a
// superseded workspace-built image (a rescan, an image-choice edit, or a
// workspace delete) can be reclaimed instead of leaking a full docker image
// forever — every resolveWorkspaceImage build lane mints a fresh, uniquely-
// named local tag on every cache miss and nothing removed the tag it
// replaced. Best-effort by design (callers log-and-continue on error, the
// same posture as ImageChecker): a substrate that pulls fresh per launch (k8s)
// has nothing local to reclaim and simply doesn't implement this.
type ImageRemover interface {
	// ImageRemove deletes ref from the substrate's local image store. A ref
	// already absent (raced by a manual prune, a prior partial cleanup) is
	// NOT an error — same idempotent-teardown contract as StopSandbox.
	ImageRemove(ctx context.Context, ref string) error
}
