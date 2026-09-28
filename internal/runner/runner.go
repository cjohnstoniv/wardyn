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
	// UserDrives reports whether this driver can BIND a member's user drive
	// (migration 0054) into the sandbox — SandboxSpec.Drive. False (fail-closed
	// default, omitempty-safe) means the control plane refuses a drive-carrying
	// request rather than admit one this driver would reject at CreateSandbox.
	UserDrives bool `json:"user_drives,omitempty"`
	// ManagedFiles reports whether this driver can deliver
	// SandboxSpec.ManagedFiles — an operator-authored file the AGENT CANNOT
	// MODIFY (root-owned, unwritable directory, present before the agent's main
	// process runs). False (fail-closed, omitempty-safe): the control plane
	// withholds the file rather than ship one the agent could rewrite.
	ManagedFiles bool `json:"managed_files,omitempty"`
	// EphemeralDiskEnforcement names WHAT ACTUALLY BINDS Resources.DiskMiB:
	// `filesystem` (docker, a storage-driver size quota), `eviction`
	// (kubernetes — the kubelet kills the pod over the limit, never refuses the
	// write) or `none`/empty (nothing binds it). The orchestrator aggregates
	// this as the WEAKEST across substrates, never the strongest. Surfaced on
	// the admin setup status, not on /healthz.
	EphemeralDiskEnforcement types.StorageEnforcement `json:"ephemeral_disk_enforcement,omitempty"`
	// Freeze reports, PER CONFINEMENT CLASS, whether this driver can
	// pause/resume the agent without losing state (long-holds design rev 4
	// §3.1). Docker/runc (CC1) is the only class verified true today;
	// runsc/Kata pause is UNVERIFIED (RL-0 spike) and never claimed here.
	// Callers (the idle/wait pause reaper, RL-7) MUST check this per the run's
	// own class rather than assume every enforced class can also pause.
	Freeze map[types.ConfinementClass]bool `json:"freeze,omitempty"`
}

// SandboxSpec is everything a driver needs to create one governed sandbox.
type SandboxSpec struct {
	RunID            uuid.UUID
	Image            string // resolved agent/workspace OCI image
	ConfinementClass types.ConfinementClass
	// Env is non-secret environment: every value here is safe to read straight
	// off the substrate's own object model (a docker container config, a k8s
	// Pod spec — which any principal with pods/get can read). Credential
	// material NEVER passes through here; it rides SecretEnv.
	Env map[string]string
	// SecretEnv is CREDENTIAL-BEARING environment, same name=value shape as
	// Env under the same names — split out because a substrate must be able to
	// deliver it WITHOUT writing the value anywhere an ordinary reader of that
	// substrate can see it. DISJOINT from Env by construction (dispatch's
	// splitSecretEnv MOVES a key, never copies), so a driver may concatenate the
	// two without deduplicating. The k8s driver MUST route these through the
	// per-run Secret via ValueFrom.SecretKeyRef (an inline EnvVar.Value is
	// readable by anyone holding pods/get in the runs namespace); the docker
	// driver passes them as ordinary container env, readable only through the
	// daemon socket — the same trust boundary docker's proxyEnv already
	// documents for the run token.
	SecretEnv map[string]string
	// ProxyConfig wires the L0 path: the sandbox's only egress is the
	// wardyn-proxy sidecar identified here.
	ProxyConfig ProxyConfig
	// Resources are hard caps (cgroups / ResourceQuota).
	Resources Resources
	// Labels are attached to the sandbox for attestation selectors and audit.
	Labels map[string]string
	// Mounts are operator/policy-controlled host bind mounts (e.g. a host repo
	// for the WSL-migration substrate, or a persistent workspace). SECURITY:
	// POLICY-controlled, NEVER attacker-controlled — the only population path
	// is internal/api dispatch copying RunPolicySpec.WorkspaceMounts; the
	// create-run wire has no mounts field. Drivers bind-mount these AND enforce
	// a deny-list defense-in-depth (runner/docker/driver.go). Default ReadOnly.
	Mounts []Mount
	// UserMountRoots, non-nil, marks a run whose mounts were authored by a
	// MEMBER (a member-owned workspace's local_dir) and carries the
	// operator/MDM-set roots those mounts must resolve inside. Resolved at
	// create-run (UserMountPolicy.RootsFor) and re-checked by the driver at BIND
	// time via ValidateUserMountSource. Nil for every operator/non-member run —
	// the member gate is purely additive and never narrows an operator mount
	// (member_mount.go).
	UserMountRoots []string
	// Drive is the acting principal's USER DRIVE (migration 0054), already
	// resolved/folded/narrowed by the control plane — nil if none was asked
	// for. NOT a Mount: it never rides WorkspaceMounts or the composer clamp,
	// and the request carried a flag rather than a path (the
	// operator-controlled-only mounts guardrail). Drivers mount ObjectName at
	// Target (DriveTarget); the Docker driver still converts it to a Mount
	// internally so the deny matrix runs on the host path.
	Drive *types.DriveMount
	// ManagedFiles are operator-authored files the AGENT CANNOT MODIFY —
	// root-owned, unwritable directory, present BEFORE the main process runs.
	// Like Mounts, POLICY-controlled and never request-set. A driver that
	// doesn't advertise Capabilities.ManagedFiles MUST refuse a spec carrying
	// them rather than start without them — silently dropping the ceiling is
	// invisible to any test that only reads the file back. See ManagedFile
	// (managed_files.go) and ValidateManagedFiles for the contract.
	ManagedFiles []ManagedFile
	// OnWaiting, non-nil, reports WHY the sandbox isn't up yet
	// (`<component>: <Reason>[: <message>]`, e.g. "agent: ImagePullBackOff: …"),
	// each time the reason CHANGES — the whole STARTING window runs inside
	// CreateSandbox with no sandbox ref yet, so nothing outside the driver can
	// ask what it's waiting on. CONTRACT: called SYNCHRONOUSLY on
	// CreateSandbox's own goroutine (must not block: wardynd's does one scoped
	// UPDATE under a 500ms deadline and drops an overdue one), only while
	// CreateSandbox is running, only on change, and DIAGNOSTIC (a driver never
	// fails a create because a report couldn't be delivered). Nil for
	// driver-level callers (conformance suite, cmd/wardyn-runner) — that's why
	// NotifyWaiting rather than the field is what drivers call.
	OnWaiting func(detail string) `json:"-"`
	// Interactive marks a run that comes up idle for `wardyn attach` (no task
	// exec'd); drivers prepare the workspace on the idle main process (e.g.
	// clone the repo) so the attach shell isn't empty. A non-interactive run
	// ignores it.
	Interactive bool
}

// NotifyWaiting delivers one OnWaiting report, or does nothing when this spec
// carries no callback. Drivers call THIS, never the field: a nil check at each
// of the four report sites is four chances to forget one.
func (s SandboxSpec) NotifyWaiting(detail string) {
	if s.OnWaiting != nil {
		s.OnWaiting(detail)
	}
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
	// MemberAuthored marks a bind whose SOURCE a MEMBER chose (a member-owned
	// workspace's local_dir). ONLY these are re-checked against
	// SandboxSpec.UserMountRoots at bind time — the spec also carries binds
	// Wardyn itself authored (credential staging dirs) and operator-owned
	// workspace dirs, none under any member root; checking those too would
	// refuse the credential mounts every model run needs. Set by internal/api
	// dispatch from the run's member-owned workspaces; false (operator default)
	// everywhere else.
	MemberAuthored bool `json:"member_authored,omitempty"`
	// DriveAuthored marks the ONE bind a driver synthesizes from
	// SandboxSpec.Drive (the host_path user drive's per-person subdirectory). A
	// LABEL, NEVER A GATE — unlike MemberAuthored, no check anywhere may be
	// written as `if m.DriveAuthored`: a drive arrives on its own field
	// (SandboxSpec.Drive), so WARDYN_USER_DRIVE_HOST_ROOTS (UserDriveHostRootCheck)
	// runs on EVERY host_path drive unconditionally regardless of this flag.
	// NEVER reaches a wire (SandboxSpec.Drive is a types.DriveMount, not a Mount,
	// precisely so the composer clamp and k8s host-bind refusal never see a
	// drive); kept as DOCUMENTATION IN THE TYPE (user-drives DESIGN §3.1(5),
	// §11 Q3). `git grep -n DriveAuthored` is the whole audit. TWO FLAGS, NOT A
	// Kind ENUM: a third `*Authored bool` is the point to consolidate into one
	// closed-set Kind field.
	DriveAuthored bool `json:"drive_authored,omitempty"`
}

type ProxyConfig struct {
	// RunToken authenticates the sidecars to the control plane (identity
	// provider verifies; it is NOT a secret usable outside the platform).
	RunToken string
	// ControlPlaneURL is where sidecars stream decisions/recordings.
	ControlPlaneURL string
	// ControlPlaneCAPEM is wardynd's internal CA (internal/hoptls): the only
	// root the sidecar trusts for ControlPlaneURL. Public; empty with a
	// loopback http URL only.
	ControlPlaneCAPEM string
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
	// ADOGrant is the run's per-person Azure DevOps grant for the proxy's REST
	// gate. See proxy.Config.ADOGrant.
	ADOGrant *proxy.ADOGrantConfig
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
	// api.Config.TrustedCAPEM), forwarded verbatim so the sidecar's egress TLS
	// additionally trusts it (never its control-plane calls). Threaded to the proxy via proxy.Config's
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
	// UpstreamProxyNoProxy is SiteConfig.UpstreamProxyNoProxy, forwarded
	// verbatim: the hosts, .domain suffixes and CIDRs whose dials skip the
	// corporate upstream proxy and are made directly. It is a ROUTING decision
	// only — a bypassed dial still faces the private-IP guard and the run's
	// policy, which is why reaching a private endpoint also needs an
	// InternalHosts declaration.
	UpstreamProxyNoProxy []string
	// LLMUpstreams maps a public vendor host to an operator-configured internal
	// model gateway base URL (api.Config.LLMGateways, forwarded verbatim;
	// WARDYN_ANTHROPIC_BASE_URL/WARDYN_OPENAI_BASE_URL). Control-plane-authored
	// (the sandbox cannot set this); empty => every brokered LLM route dials
	// the vendor host, byte-identical to today. Threaded to the proxy via
	// proxy.Config's identically-named field (BuildProxyConfig below).
	LLMUpstreams map[string]string
	// LLMUnavailableDetail is the control-plane-composed reason the proxy's
	// brokered-LLM 404 gives when this run has no LLM credential behind that
	// route (internal/api's llmUnavailableDetail). Empty => the route's own
	// generic detail. Threaded to the proxy via proxy.Config's identically-named
	// field (BuildProxyConfig below).
	LLMUnavailableDetail string
	// Unattended marks a run nobody is driving (a non-interactive task run):
	// a push its push_rules would hold for review is refused instead, since
	// there is nobody to ask. See proxy.Config.Unattended.
	Unattended bool
}

// InjectionGrant pairs an api_key credential grant with its proxy-side
// injection rule (host/header/format/secret name — never the secret value).
type InjectionGrant struct {
	GrantID uuid.UUID            `json:"grant_id"`
	Rule    egress.InjectionRule `json:"rule"`
}

// Resources are the hard sandbox caps the driver applies as cgroup/storage
// limits. A ZERO field means "use the driver's conservative platform
// default" (the docker driver fills CPU/memory/PIDs unconditionally so
// EVERY sandbox is capped even when policy sets nothing). Copied from a
// policy's types.ResourceLimits at dispatch.
type Resources struct {
	CPUMillis int64
	MemoryMiB int64
	// PidsLimit caps processes/threads in the sandbox (fork-bomb guard). Zero
	// => driver default.
	PidsLimit int64
	// DiskMiB caps writable storage. Best-effort, and WHAT BINDS IT DIFFERS BY
	// SUBSTRATE (Capabilities.EphemeralDiskEnforcement names which): docker
	// applies a storage-driver quota only when supported (overlay2 on
	// xfs+pquota, or btrfs/zfs), else warns and runs uncapped; k8s sets
	// resources.limits[ephemeral-storage], enforced by the kubelet EVICTING
	// the pod (never refusing the write).
	DiskMiB int64
	// DiskMiBFilled says DiskMiB was FILLED IN from the org's
	// storage.ephemeral.default_disk_mib rather than authored on its policy —
	// the bit that tells a driver whose host can't enforce a cap whether
	// refusing the run is honest or destructive. The desktop tier
	// (overlay2/ext4) fails the create closed for a POLICY-AUTHORED DiskMiB,
	// but a FILLED org default degrades to uncapped-with-a-warning instead —
	// failing closed on a fleet-wide MDM default would brick every
	// request-less run the moment an admin set a number for the k8s half of
	// the estate. k8s ignores this bit: eviction enforces either way, with no
	// unenforceable case to degrade.
	DiskMiBFilled bool
}

// AttachOptions configures an interactive attach. Cols/Rows are the initial PTY
// window size; zero values let the driver pick a sane default (e.g. 80x24).
type AttachOptions struct {
	Cols uint16
	Rows uint16
}

// Session is a live, bidirectional interactive PTY stream into a RUNNING
// sandbox, opened by Runner.Attach — the human-facing analogue of the agent
// exec.
//
// SECURITY (invariant 3): runs INSIDE the existing sandbox, bounded by the
// same L0 confinement envelope as the agent; opens NO new network path (the
// stream flows control-plane -> dockerd -> container, never through the
// sandbox's HTTP_PROXY egress path). Attach grants a terminal, not a new
// egress route.
//
// Not safe for concurrent Read/Write from multiple goroutines on the same
// direction; running Read and Write each in their own goroutine is
// supported.
type Session interface {
	// Read copies terminal output into p; io.EOF when the shell exits or the
	// stream is closed.
	Read(p []byte) (int, error)
	// Write sends keystrokes into the shell.
	Write(p []byte) (int, error)
	// Resize informs the PTY of a new window size.
	Resize(ctx context.Context, cols, rows uint16) error
	// Close tears down ONLY the interactive exec stream — not the sandbox,
	// the agent process, or any sidecar.
	Close() error
}

// ErrExecStreamUnsupported is the sentinel a Runner/Substrate returns from
// ExecStream when unimplemented. The conformance suite errors.Is against
// this exact sentinel to skip cleanly on "not implemented" while still
// FAILING on any other error.
var ErrExecStreamUnsupported = errors.New("runner: ExecStream not supported")

// ExecSpec describes one exec launched via Runner.ExecStream: the argv to run,
// exec-scoped environment, and whether it runs under a PTY.
type ExecSpec struct {
	Argv []string
	// Env is additional exec-scoped environment (on top of the sandbox's own).
	Env []string
	// TTY requests a pseudo-terminal, which MERGES stdout and stderr onto
	// ExecSession.Stdout (Stderr then reads io.EOF immediately). Without a
	// TTY, stdout/stderr are SEPARATE streams — a binary protocol on stdout
	// (SFTP, socat) would be corrupted by interleaved stderr, so merging is
	// only acceptable under PTY semantics.
	TTY bool
	// Cols, Rows seed the initial PTY window size (TTY only); zero lets the
	// implementation pick a default.
	Cols, Rows uint16
}

// ExecSession is a live, bidirectional stream into ONE exec started by
// Runner.ExecStream — the streaming-primitive analogue of Session. A PLAIN
// STRUCT of streams/closures, deliberately NEVER an interface: a fake can
// construct a partial ExecSession (a canned Wait, a strings.Reader for
// Stdout, a nil Stdin) by filling in fields directly, with no method set to
// satisfy wrong.
//
// Resize, Wait, and Close take NO context: an implementation binds them to
// whatever context it created the exec with.
type ExecSession struct {
	// Stdin writes to the exec's stdin. Close HALF-closes the write side only
	// (the exec observes EOF) — implementations MUST NOT tear down
	// Stdout/Stderr when Stdin.Close is called.
	Stdin io.WriteCloser
	// Stdout carries standard output; with ExecSpec.TTY=true it ALSO carries
	// stderr (PTY semantics merge the two); with TTY=false it carries ONLY
	// stdout — see Stderr.
	Stdout io.Reader
	// Stderr carries standard error as a SEPARATE stream when TTY is false;
	// when TTY is true it reads io.EOF immediately (no separate channel).
	//
	// STREAMING CONTRACT (TTY=false): Stdout and Stderr are UNBUFFERED
	// io.Pipes fed by one background demux goroutine off ONE connection — an
	// undrained stderr byte blocks the demux, Stdout, AND Wait. Callers MUST
	// start draining Stderr BEFORE or concurrently with the first Stdout
	// read (as with any demultiplexed docker/moby attach stream).
	Stderr io.Reader
	// Resize changes the PTY window size; a no-op returning nil with no TTY.
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

// ErrExecNeverStarted: a Runner's Wait returns this when it can prove the
// agent exec will NEVER reach a terminal state on its own (e.g. k8s's
// ephemeral agent container stuck on a hard-failure Reason like
// ImagePullBackOff). Distinct from every other Wait error (transient probe
// error, ctx cancellation), which mean "might still be running, retry" — this
// one means the caller (startCompletionWatcher) must fail the run
// immediately.
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
	// recording); returns once started, not finished. agentExecID identifies
	// the started process for exec-based substrates ("" for exec-less/
	// main-process substrates, where the container IS the agent) — persist it
	// so the crash reconciler can observe agent liveness across a restart via
	// AgentStatus.
	Exec(ctx context.Context, ref string, argv []string) (agentExecID string, err error)
	// Wait blocks until the agent process Exec started has exited, returning
	// its exit code. Valid ONLY after a successful Exec on the same ref;
	// errors if no agent exec is tracked for ref.
	Wait(ctx context.Context, ref string) (exitCode int, err error)
	// Attach opens a NEW interactive shell inside the already-RUNNING sandbox
	// ref, SEPARATE from the agent process Wait tracks (attaching/detaching
	// never affects the agent). Session.Close tears down ONLY the exec
	// stream, not the sandbox. Bounded by the SAME L0 egress envelope as the
	// agent (invariant 3); callers MUST record the human principal for
	// attribution (invariant 4) at the call site.
	Attach(ctx context.Context, ref string, opts AttachOptions) (Session, error)
	// ExecStream launches spec.Argv inside the running sandbox as a fresh,
	// streamable exec, distinct from both Exec's agent process and Attach's
	// interactive shell (argv runs directly, no PTY/shell wrapping). UNLIKE
	// Exec, MUST be repeatable against the SAME ref (an SSH/SFTP bridge opens
	// one ExecStream per channel against one long-lived ref). k8s implements
	// it on the streaming exec subresource, not an ephemeral container (see
	// Substrate.Exec's doc). SECURITY: opens no new network path (invariant
	// 3); callers MUST record the human principal (invariant 4) — a raw argv
	// makes this the MORE dangerous of the two streaming primitives.
	ExecStream(ctx context.Context, ref string, spec ExecSpec) (*ExecSession, error)
	Status(ctx context.Context, ref string) (Status, error)
	// AgentStatus reports the AGENT's observed state restart-safely, given the
	// persisted agentExecID. For exec-based substrates it inspects that exec,
	// so a run whose agent exited reports terminal State+ExitCode even while
	// the idle container is still up. Falls back to Status when agentExecID
	// is "" (exec-less/main-process, or Exec never ran).
	AgentStatus(ctx context.Context, ref, agentExecID string) (Status, error)
	// StopSandbox is the graceful path (lifecycle auto-stop).
	StopSandbox(ctx context.Context, ref string) error
	// KillSandbox is the kill-switch path: immediate teardown. The control
	// plane cascades identity + credential revocation around this call.
	KillSandbox(ctx context.Context, ref string) error
}

// SandboxEnder is an OPTIONAL Runner capability: stop a sandbox and KEEP it
// (the lease end, long-holds design rev 4). EndSandbox stops the agent without
// removing it, so its files survive, and stops the proxy sidecar, so nothing
// the agent could restart has a network path. StopSandbox/KillSandbox still
// tear the kept sandbox down later. Idempotent on a missing sandbox.
//
// A substrate that cannot keep a stopped sandbox (Kubernetes: stopping a pod
// deletes it) does not implement it, and a router in front of one returns
// ErrEndUnsupported; the control plane then stops the run outright.
type SandboxEnder interface {
	EndSandbox(ctx context.Context, ref string) error
}

// ErrEndUnsupported is EndSandbox's (and StopProxy's) answer from a router
// whose substrate for ref cannot keep a stopped (or lost) sandbox.
var ErrEndUnsupported = errors.New("runner: this substrate cannot keep an ended sandbox")

// ProxyStopper is an OPTIONAL Runner capability: stop a sandbox's proxy
// sidecar and leave its agent running (a run lost to a control-plane outage,
// long-holds design rev 4 §4 row 2). The agent keeps its processes and files
// but has no network path, because the proxy was its only one. The stopped
// proxy is removed, not kept: nothing a revive needs lives in it (the control
// plane stores the run's proxy config, #1176), and a stopped container would
// hold nothing but its own leftovers. Idempotent on a missing or
// already-stopped proxy; an unresolvable ref is an error, never a success that
// left the proxy up. A graceful stop that fails escalates to a kill; a proxy
// that survives both is an error, with the agent stopped too
// (kept, never removed) so no work runs while its egress is unconfirmed. An
// error means containment is unconfirmed, not that the sandbox may go: the
// control plane keeps the run and retries (#1060). A router in front of a
// substrate without it returns ErrEndUnsupported.
type ProxyStopper interface {
	StopProxy(ctx context.Context, ref string) error
}

// ProxyReviver is an OPTIONAL Runner capability: replace a sandbox's proxy
// sidecar, running or stopped, with a new one while the agent keeps running
// (proxy-only revive and restart with current limits, long-holds design rev 4
// §4.1). The control plane reads the run's stored config (never the proxy
// container, #1176), rewrites only its token and its denies, and hands it to
// ReplaceProxy; the per-run MITM CA inside it is carried over.
//
// CanReplaceProxy answers ErrReviveUnsupported, before the caller claims the
// run, when ref's substrate cannot replace a proxy in place, and nil
// otherwise. ReplaceProxy removes the old proxy (if there still is one), then
// starts the new one on the run's network at the address the agent's hosts
// entry pins, delivering cfgJSON so that no container config or environment
// holds it. An error wrapping ErrProxyReplaceFailed means the old proxy is, or
// may be, gone and no new one runs: the sandbox has no egress, and the caller
// must treat the run as lost; a later revive rebuilds from the stored config.
// Any other error came before the old proxy was touched and left it as it
// was. A router in front of a substrate without it (Kubernetes: the agent pins
// the proxy pod's IP) returns ErrReviveUnsupported.
type ProxyReviver interface {
	CanReplaceProxy(ctx context.Context, ref string) error
	ReplaceProxy(ctx context.Context, ref string, cfgJSON []byte) error
	// EnsureProxyImage pulls the proxy sidecar image if it is not already
	// present locally. The control plane calls this BEFORE the revive claim
	// (long-holds design rev 4 §4.1; F2, Fable review): a slow first pull then
	// happens while the run is still marked lost, so the window the claim
	// opens — during which a watcher sweep must leave the run alone rather
	// than lose it again — covers only a fast remove+create+start, never an
	// image pull.
	EnsureProxyImage(ctx context.Context) error
}

// ErrReviveUnsupported is ProxyReviver's answer from a router whose substrate
// for ref cannot replace a proxy in place.
var ErrReviveUnsupported = errors.New("runner: this substrate cannot replace a sandbox's proxy")

// ErrProxyReplaceFailed marks a ReplaceProxy that removed, or may have
// removed, the old proxy and did not start the new one.
var ErrProxyReplaceFailed = errors.New("runner: the old proxy may be gone and the new one did not start")

// SandboxStarter is an OPTIONAL Runner capability: start a kept sandbox's
// stopped agent again (revive after a reboot, long-holds design rev 4 §4 row
// 3). The agent comes back with its writable layer, so its files and the
// harness transcript survive; its main process is re-run from the start, so
// nothing that was running does. It never starts the proxy sidecar: the
// caller replaces that first (ProxyReviver), and StartSandbox refuses unless
// it is running, so the agent never runs behind the old proxy or without the
// address its hosts entry pins. Idempotent on an agent already running. A
// router in front of a substrate without it (Kubernetes: a stopped pod is
// gone) returns ErrReviveUnsupported.
type SandboxStarter interface {
	StartSandbox(ctx context.Context, ref string) error
}

// Freezer is an OPTIONAL Runner capability: pause and resume the AGENT
// container in place, without stopping it (runner Freeze/Thaw, long-holds
// design rev 4 §3). FreezeSandbox pauses the agent process — its memory,
// disk and any already-established TCP connection keep their state, and the
// daemon refuses a new exec against it until thawed. ThawSandbox resumes it.
// Both are idempotent: a missing sandbox, or a redundant call (freezing an
// already-frozen one, thawing a running one), returns nil, the same
// tolerant-of-a-retried-signal contract Stop/Kill hold for a gone sandbox.
//
// The PROXY SIDECAR IS NEVER FROZEN — only the ref this is called with (the
// agent). The proxy keeps renewing its run token and answering egress
// decisions while the agent is paused; a caller wanting the proxy left alone
// gets that for free by calling this with only the agent's ref.
//
// A substrate with no pause primitive at all (Kubernetes: stopping a pod is
// the only lever) does not implement this, and a router in front of one
// returns ErrFreezeUnsupported; the caller then leaves the run running
// rather than silently no-op a pause nobody can prove happened.
//
// Implementing this interface is NOT itself proof the pause is safe for
// every runtime the substrate carries: runsc/Kata run inside the Docker
// substrate, which does implement Freezer, but their pause is UNVERIFIED
// (Capabilities.Freeze[class] is false for them). The caller MUST check
// Capabilities.Freeze for the sandbox's confinement class before calling —
// the interface assertion alone does not gate this.
type Freezer interface {
	FreezeSandbox(ctx context.Context, ref string) error
	ThawSandbox(ctx context.Context, ref string) error
}

// ErrFreezeUnsupported is Freezer's answer from a router whose substrate for
// ref cannot pause it.
var ErrFreezeUnsupported = errors.New("runner: this substrate cannot freeze a sandbox")

// ImageChecker is an OPTIONAL Runner capability: a
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

// DriveProbeResult is the closed set of answers a DriveProber gives about one
// resolved drive mount. Three states, not a bool, because "I checked and it
// is fine" and "I could not tell" are different claims with different
// remedies — see DriveProbeUnknown.
type DriveProbeResult string

const (
	// DriveProbeReadable: the probe ran AS THE AGENT'S OWN UID (never the
	// daemon's process, which is root) and that uid could read the mount.
	DriveProbeReadable DriveProbeResult = "readable"
	// DriveProbeUnreadable: the probe ran as the agent's own uid and that uid
	// could NOT read the mount — the exact failure a daemon-side os.Stat (run
	// as root) cannot see, because root can read almost anything the agent
	// user cannot.
	DriveProbeUnreadable DriveProbeResult = "unreadable"
	// DriveProbeUnknown: the probe could not be run to a conclusion — e.g. the
	// Kubernetes substrate has no filesystem of its own to stat and can only
	// Get the claim and read its phase, which is a fact about provisioning,
	// not about whether the agent uid can read it once mounted. A caller MUST
	// NOT treat Unknown as DriveProbeReadable: a probe that cannot see the
	// storage has not proved anything, and reading Unknown as a pass would
	// re-introduce the exact bug this interface exists to close.
	DriveProbeUnknown DriveProbeResult = "unknown"
)

// DriveProbe is a DriveProber's answer for one resolved mount.
type DriveProbe struct {
	Result DriveProbeResult
	// Detail is operator-facing context on why the probe landed here (an exec
	// exit code, a claim phase) — logged, never shown to the member.
	Detail string
}

// DriveProber is an OPTIONAL Runner capability, modelled on ImageChecker: a
// substrate that can ask whether the SANDBOX'S OWN USER — not the daemon's own
// process, which is root — can actually read a resolved user-drive mount.
//
// It exists because the inline os.Stat a daemon runs itself
// (internal/api/user_drives_run.go, pre-#165) always runs as root, so a share
// readable by root but not by the agent uid passed create, preflight and /me
// and only failed once the run was already inside the sandbox — and on
// Kubernetes there was no filesystem for the daemon to stat at all.
//
// An OPTIONAL capability rather than a widening of Runner: five
// implementations satisfy Runner today, mounting a drive five different ways,
// and a new required method would have to be stubbed everywhere it means
// nothing. Callers type-assert the wired Runner and treat "does not
// implement" the same as ImageChecker's absence — the check simply does not
// run, which is exactly what the code answered before this interface existed.
type DriveProber interface {
	// ProbeDrive answers whether mount would be readable by the uid the
	// sandbox actually runs as, bounded by ctx. It is called BEFORE a sandbox
	// exists (create, preflight, a /me poll) and MUST honour ctx's deadline —
	// the caller is a request thread, not a background sweep.
	ProbeDrive(ctx context.Context, mount types.DriveMount) (DriveProbe, error)
}

// DriveReclaimOutcome is the closed set of answers a DriveReclaimer gives for
// one object it was asked to destroy. Two states, and the second is not an
// error: the object being gone already is the same END STATE the caller asked
// for, reached by a prior partial reclaim or by the operator's own
// `docker volume rm` / `kubectl delete pvc` — the idempotent-teardown contract
// StopSandbox already takes. Telling the two apart matters only to the audit
// row, which is exactly why it is a value and not a bool.
type DriveReclaimOutcome string

const (
	// DriveReclaimDeleted: this call issued the delete and the substrate
	// accepted it. The bytes are gone.
	DriveReclaimDeleted DriveReclaimOutcome = "deleted"
	// DriveReclaimAlreadyAbsent: no object answered to that name, so this call
	// destroyed nothing. Not an error — but never reported as "deleted"
	// either, because an audit row that says a person's storage was destroyed
	// when it was already missing is the one row an operator must be able to
	// trust.
	DriveReclaimAlreadyAbsent DriveReclaimOutcome = "already_absent"
)

// ErrDriveInUse is the sentinel a DriveReclaimer returns when a sandbox still
// holds the object: a running container mounts the Docker volume, or a pod
// still references the claim. The caller answers 409 and the operator retries
// once the run has finished.
//
// A refusal rather than a force-delete, and the asymmetry is deliberate: on
// Docker a forced remove would pull the volume out from under a live agent
// mid-write, and on Kubernetes the apiserver ACCEPTS a delete against an
// in-use claim and leaves it Terminating behind the pvc-protection finalizer —
// which destroys nothing now and refuses the member's NEXT run with
// errDriveClaimTerminating until the pod goes. Neither is "reclaimed".
var ErrDriveInUse = errors.New("runner: the drive's storage is still held by a running sandbox")

// ErrDriveNotReclaimable is the sentinel a DriveReclaimer returns when the
// object that answers to the drive's name is NOT the storage this drive
// allocated — another drive's object under a colliding minted name, another
// principal's object under a home template that folds two people onto one, an
// operator's own pre-existing object, or one already being deleted.
//
// The same identity evidence the mount path refuses on (driveClaimIdentity /
// driveVolumeAdoptable), asked one last time before anything is destroyed:
// a mount that gets identity wrong shows one member another member's files,
// and a reclaim that gets it wrong deletes them.
var ErrDriveNotReclaimable = errors.New("runner: the object under this drive's name is not the storage it allocated")

// DriveReclaimer is an OPTIONAL Runner capability, modelled on ImageRemover:
// a substrate that can DESTROY the per-person storage object a user drive
// allocated.
//
// It is the one verb in this file that is irreversible, and it exists because
// there was no verb at all: deleting a drive removed its row and left the
// volume or the claim behind, with nothing in the product able to name it
// afterwards. The alternative — a daemon that reclaims on its own, at teardown
// or when an allocation goes away — is refused outright: a drive OUTLIVES
// every run that mounts it, so no automatic path may ever reach this.
//
// On Kubernetes the verb is not even granted by default. The chart's Role
// carries `persistentvolumeclaims: [get, create]` and gains `delete` only
// under `drives.reclaim.enabled`, so a stock install cannot execute this
// call at all and the apiserver's own 403 is the backstop under the API's
// super-admin gate.
type DriveReclaimer interface {
	// ReclaimDrive destroys the storage object mount names, bounded by ctx.
	//
	// It MUST refuse rather than destroy when the object is not this drive's
	// (ErrDriveNotReclaimable) or is still held by a sandbox (ErrDriveInUse),
	// and it MUST answer DriveReclaimAlreadyAbsent — not an error — when
	// nothing answers to the name.
	ReclaimDrive(ctx context.Context, mount types.DriveMount) (DriveReclaimOutcome, error)
}
