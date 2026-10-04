// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runner defines the target-agnostic sandbox lifecycle contract. The
// control plane ONLY talks to this interface — zero Docker/Kubernetes-specific
// code. Drivers live in subpackages and are conformance-tested identically.
package runner

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// Capabilities declares what a driver (on this host/cluster) can actually
// enforce. The control plane refuses to schedule a run whose policy demands
// more than the driver declares. Never claim a control that is not
// structurally enforced.
type Capabilities struct {
	Driver                    string                            `json:"driver"`                                // e.g. "docker"
	ConfinementClasses        []types.ConfinementClass          `json:"confinement_classes"`                   // strongest last
	Resolved                  map[types.ConfinementClass]string `json:"confinement_substrates,omitempty"`      // substrate label per class, for /healthz
	StructuralEgress          bool                              `json:"structural_egress"`                     // L0: no default route, sole egress = wardyn-proxy
	NetworkPolicy             bool                              `json:"network_policy"`                        // L1 (nftables / NetworkPolicy default-deny)
	NetworkPolicyAcknowledged bool                              `json:"network_policy_acknowledged,omitempty"` // operator accepted a canary failure as expected rather than proven
	SessionRecording          bool                              `json:"session_recording"`                     // wardyn-rec sidecar support
	// UserDrives and ManagedFiles are fail-closed: false means the control
	// plane refuses rather than admit a request the driver would reject.
	UserDrives   bool `json:"user_drives,omitempty"`
	ManagedFiles bool `json:"managed_files,omitempty"` // can deliver SandboxSpec.ManagedFiles (root-owned, AGENT CANNOT MODIFY)
	// EphemeralDiskEnforcement names what binds Resources.DiskMiB:
	// `filesystem` (docker quota), `eviction` (k8s, never refuses the write),
	// or `none`/empty. Aggregated as the WEAKEST across substrates.
	EphemeralDiskEnforcement types.StorageEnforcement `json:"ephemeral_disk_enforcement,omitempty"`
	// Freeze reports, PER CLASS, pause/resume support; runsc/Kata pause is
	// UNVERIFIED and never claimed here. Callers MUST check per the run's class.
	Freeze map[types.ConfinementClass]bool `json:"freeze,omitempty"`
}

// SandboxSpec is everything a driver needs to create one governed sandbox.
type SandboxSpec struct {
	RunID            uuid.UUID
	Image            string // resolved agent/workspace OCI image
	ConfinementClass types.ConfinementClass
	// Env is non-secret environment. Credential material NEVER passes through
	// here; it rides SecretEnv.
	Env map[string]string
	// SecretEnv is CREDENTIAL-BEARING environment, DISJOINT from Env. The k8s
	// driver MUST route these through the per-run Secret via
	// ValueFrom.SecretKeyRef (an inline EnvVar.Value is readable by anyone
	// holding pods/get); docker passes them as container env, readable only
	// through the daemon socket — the same trust boundary as the run token.
	SecretEnv map[string]string
	// ProxyConfig wires the L0 path: the sandbox's only egress is the
	// wardyn-proxy sidecar identified here.
	ProxyConfig ProxyConfig
	Resources   Resources         // hard caps (cgroups / ResourceQuota)
	Labels      map[string]string // attached for attestation selectors and audit
	// Mounts are operator/policy-controlled host bind mounts. SECURITY:
	// POLICY-controlled, NEVER attacker-controlled. Drivers bind-mount these
	// AND enforce a deny-list defense-in-depth. Default ReadOnly.
	Mounts []Mount
	// UserMountRoots, non-nil, marks a run whose mounts were MEMBER-authored
	// and carries the roots those mounts must resolve inside, re-checked at
	// BIND time. Nil elsewhere — never narrows an operator mount.
	UserMountRoots []string
	// Drive is the acting principal's USER DRIVE, already resolved by the
	// control plane — nil if none was asked for. NOT a Mount: the request
	// carried a flag rather than a path.
	Drive *types.DriveMount
	// ManagedFiles are operator-authored files the AGENT CANNOT MODIFY —
	// root-owned, unwritable, present BEFORE the main process runs.
	// POLICY-controlled. A driver that doesn't advertise
	// Capabilities.ManagedFiles MUST refuse rather than start without them.
	ManagedFiles []ManagedFile
	// OnWaiting, non-nil, reports WHY the sandbox isn't up yet, each time the
	// reason CHANGES. Called SYNCHRONOUSLY on CreateSandbox's own goroutine
	// (must not block). Call NotifyWaiting, never the field.
	OnWaiting func(detail string) `json:"-"`
	// Interactive marks a run that comes up idle for `wardyn run attach`; a
	// driver prepares the workspace on the idle main process for it.
	Interactive bool
	// ExecOutput, non-nil, receives a copy of the agent exec's combined
	// stdout/stderr (GET /runs/{id}/output). Its Write must never block or
	// fail: the driver drains the exec through it.
	ExecOutput io.Writer `json:"-"`
}

// NotifyWaiting delivers one OnWaiting report, or does nothing when this spec
// carries no callback. Drivers call THIS, never the field.
func (s SandboxSpec) NotifyWaiting(detail string) {
	if s.OnWaiting != nil {
		s.OnWaiting(detail)
	}
}

// Mount is one operator/policy-controlled host bind mount into the sandbox.
// Source is a host path; Target is the in-container path. ReadOnly defaults
// to true. See SandboxSpec.Mounts for the security model.
type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
	// MemberAuthored marks a bind whose SOURCE a MEMBER chose. ONLY these are
	// re-checked against SandboxSpec.UserMountRoots at bind time.
	MemberAuthored bool `json:"member_authored,omitempty"`
	// DriveAuthored marks the ONE bind a driver synthesizes from
	// SandboxSpec.Drive. A LABEL, NEVER A GATE: WARDYN_USER_DRIVE_HOST_ROOTS
	// runs on EVERY host_path drive regardless of this flag.
	DriveAuthored bool `json:"drive_authored,omitempty"`
}

type ProxyConfig struct {
	RunToken          string // authenticates the sidecars to the control plane (NOT usable outside the platform)
	ControlPlaneURL   string // where sidecars stream decisions/recordings
	ControlPlaneCAPEM string // wardynd's internal CA; empty with a loopback http URL only
	// Policy is the run's egress policy, handed verbatim to the wardyn-proxy
	// sidecar. Drivers MUST deliver it at launch: a proxy without a policy
	// fails closed and the sandbox has no working egress at all.
	Policy    types.RunPolicySpec
	Injection []InjectionGrant // auto-mintable api_key grants the proxy resolves at startup (values live only in proxy memory)
	// MITMCACertPEM / MITMCAKeyPEM are the OPTIONAL per-run TLS-MITM CA. The
	// private key reaches ONLY the proxy, never the sandbox. Empty => opaque
	// CONNECT passthrough.
	MITMCACertPEM string
	MITMCAKeyPEM  string
	MITMHosts     []string                  // OPERATOR-CONFIGURED corp hosts the proxy may TLS-MITM; NEVER attacker-controlled
	MITMLLM       bool                      // TLS-MITM of the built-in LLM hosts intended for this run; see proxy.Config.MITMLLM
	GitGrants     map[string]uuid.UUID      // git-broker per-repo allowlist; token stays proxy-side
	PATGrants     map[string]proxy.PATGrant // git_pat broker's per-HOST allowlist; PAT minted proxy-side, never enters the sandbox
	ADOGrant      *proxy.ADOGrantConfig     // per-person Azure DevOps grant for the proxy's REST gate
	// AzureGates are the run's azure_foundry route gates. A non-empty set also sizes the sidecar's
	// memory envelope up (ProxyLimitsFor): the in-flight body budget does not fit the default.
	AzureGates []proxy.AzureGateConfig
	// BrokeredPATGrantIDs is every git_pat grant id of the run while the PAT broker is on; the proxy refuses a raw mint of one.
	BrokeredPATGrantIDs []uuid.UUID
	// UpstreamProxyURL is the OPTIONAL corporate parent proxy the sidecar
	// chains egress through; control-plane calls bypass it. Empty => direct dial.
	UpstreamProxyURL string
	// TrustedCAPEM is the operator's corporate CA bundle, forwarded so the
	// sidecar's egress TLS additionally trusts it. Empty => system roots only.
	TrustedCAPEM  string
	InternalHosts []types.InternalHost // operator-declared internal hostnames eligible for the proxy's private-IP-guard lift
	// UpstreamProxyNoProxy: hosts/CIDRs whose dials skip the corporate
	// upstream proxy — still faces the private-IP guard and the run's policy.
	UpstreamProxyNoProxy []string
	LLMUpstreams         map[string]string // public vendor host -> operator model gateway base URL
	LLMChannelHosts      map[string]string // model host -> vendor schema it is inspected as; never a gateway (proxy.Config.LLMChannelHosts)
	LLMUnavailableDetail string            // reason for the brokered-LLM 404 when no credential backs it
	Unattended           bool              // a run nobody is driving: a held push is refused instead
	Attribution          *policyref.Ref    // the policy named in a policy-decided refusal; nil when none
}

// InjectionGrant pairs an api_key grant with its proxy-side injection rule (never the secret value).
type InjectionGrant struct {
	GrantID uuid.UUID            `json:"grant_id"`
	Rule    egress.InjectionRule `json:"rule"`
}

// Resources are the hard sandbox caps the driver applies as cgroup/storage
// limits. A ZERO field means "use the driver's conservative platform default".
type Resources struct {
	CPUMillis int64
	MemoryMiB int64
	// CPURequestMillis/MemoryRequestMiB are the k8s scheduling requests; 0 means
	// "same as the limit". Never policy-authored: EffectiveRequests fills them from
	// the deployment ratio. The Docker substrate ignores them.
	CPURequestMillis int64
	MemoryRequestMiB int64
	PidsLimit        int64 // fork-bomb guard; zero => driver default
	// DiskMiB caps writable storage; WHAT BINDS IT DIFFERS BY SUBSTRATE (see
	// Capabilities.EphemeralDiskEnforcement): docker quotas only when
	// supported, else uncapped; k8s enforces via kubelet EVICTION.
	DiskMiB int64
	// DiskMiBFilled says DiskMiB was FILLED IN from the org default rather
	// than policy-authored: the desktop tier fails closed only for a
	// POLICY-AUTHORED value; a FILLED default degrades to uncapped instead.
	DiskMiBFilled bool
}

// AttachOptions configures an interactive attach; zero Cols/Rows let the driver pick a sane default (e.g. 80x24).
type AttachOptions struct {
	Cols uint16
	Rows uint16
	// Observer marks a client that only watches the shared tmux session. Its
	// tmux client is attached with the ignore-size flag (tmux >= 3.2), so it is
	// never counted when tmux sizes the shared window; on older tmux it is
	// seeded from the writer's live size instead (Cols/Rows).
	Observer bool
}

// Session is a live, bidirectional interactive PTY stream into a RUNNING
// sandbox, opened by Runner.Attach.
//
// SECURITY (invariant 3): runs INSIDE the existing sandbox, bounded by the
// same L0 confinement envelope as the agent; opens NO new network path.
// Attach grants a terminal, not a new egress route.
//
// Not safe for concurrent Read/Write from multiple goroutines on the same
// direction; Read and Write each in their own goroutine is supported.
type Session interface {
	Read(p []byte) (int, error)                          // io.EOF when the shell exits or closes
	Write(p []byte) (int, error)                         // sends keystrokes
	Resize(ctx context.Context, cols, rows uint16) error // new PTY window size
	// Close tears down ONLY the interactive exec stream — not the sandbox,
	// the agent process, or any sidecar.
	Close() error
}

// ErrExecStreamUnsupported is the sentinel a Runner/Substrate returns from
// ExecStream when unimplemented, so the conformance suite can skip cleanly.
var ErrExecStreamUnsupported = errors.New("runner: ExecStream not supported")

// ErrSandboxGone is what a Runner/Substrate wraps around an ExecStream failure
// whose cause is that the sandbox (the pod or container) no longer exists: a
// finishing run's sandbox is torn down a moment before its state flips, and an
// exec into it is not a fault of the read.
var ErrSandboxGone = errors.New("runner: the sandbox is gone")

// ExecSpec describes one exec launched via Runner.ExecStream: the argv to run,
// exec-scoped environment, and whether it runs under a PTY.
type ExecSpec struct {
	Argv []string
	Env  []string // additional exec-scoped environment, on top of the sandbox's own
	// TTY requests a pseudo-terminal, which MERGES stdout and stderr onto
	// ExecSession.Stdout — a binary protocol on stdout (SFTP, socat) would be
	// corrupted by interleaved stderr without one.
	TTY        bool
	Cols, Rows uint16 // initial PTY window size (TTY only); zero => implementation default
}

// ExecSession is a live, bidirectional stream into ONE exec started by
// Runner.ExecStream. A PLAIN STRUCT of streams/closures, deliberately NEVER
// an interface: a fake can construct a partial ExecSession by filling in
// fields directly. Resize, Wait, and Close take NO context: an implementation
// binds them to whatever context it created the exec with.
type ExecSession struct {
	// Stdin writes to the exec's stdin. Close HALF-closes the write side only
	// — implementations MUST NOT tear down Stdout/Stderr when Stdin.Close is
	// called.
	Stdin io.WriteCloser
	// Stdout carries standard output; with ExecSpec.TTY=true it ALSO carries
	// stderr — see Stderr.
	Stdout io.Reader
	// Stderr carries standard error as a SEPARATE stream when TTY is false.
	// STREAMING CONTRACT: Stdout and Stderr are UNBUFFERED io.Pipes fed by one
	// demux goroutine off ONE connection — an undrained stderr byte blocks the
	// demux, Stdout, AND Wait. Callers MUST drain Stderr concurrently.
	Stderr io.Reader
	Resize func(cols, rows uint16) error // no-op returning nil with no TTY
	Wait   func() (int, error)
	Close  func() error // tears down ONLY this exec stream — never the sandbox, agent, or any sidecar
}

// Sandbox is a handle to a created sandbox.
type Sandbox struct {
	Ref           string // container ID / pod name
	Driver        string
	EnforcedClass types.ConfinementClass // what the driver actually applied (>= requested or error)
}

// Status reports observed sandbox state.
type Status struct {
	State    types.RunState
	ExitCode *int
	Message  string
}

// ErrExecNeverStarted: a Runner's Wait returns this when it can prove the
// agent exec will NEVER reach a terminal state (e.g. ImagePullBackOff) —
// distinct from every other Wait error ("might still be running, retry").
var ErrExecNeverStarted = errors.New("runner: agent exec never started")

// Runner is the lifecycle contract. Implementations must be safe for
// concurrent use. Every method must be idempotent where the verb implies it
// (Stop/Kill on a gone sandbox return nil).
type Runner interface {
	Name() string
	Capabilities(ctx context.Context) (Capabilities, error)
	CreateSandbox(ctx context.Context, spec SandboxSpec) (Sandbox, error) // L0 confinement: no default route, egress only via the proxy
	// Exec starts the agent process; returns once started, not finished.
	// Persist agentExecID for restart-safe liveness via AgentStatus.
	Exec(ctx context.Context, ref string, argv []string) (agentExecID string, err error)
	Wait(ctx context.Context, ref string) (exitCode int, err error) // blocks until Exec's process exits; valid only after Exec on the same ref
	// Attach opens a NEW interactive shell, SEPARATE from the agent process.
	// Bounded by the SAME L0 egress envelope (invariant 3); callers MUST
	// record the human principal (invariant 4).
	Attach(ctx context.Context, ref string, opts AttachOptions) (Session, error)
	// ExecStream launches spec.Argv as a fresh, streamable exec. UNLIKE Exec,
	// MUST be repeatable against the SAME ref. SECURITY: opens no new network
	// path (invariant 3); callers MUST record the human principal (invariant 4).
	ExecStream(ctx context.Context, ref string, spec ExecSpec) (*ExecSession, error)
	Status(ctx context.Context, ref string) (Status, error)
	AgentStatus(ctx context.Context, ref, agentExecID string) (Status, error) // restart-safe; falls back to Status when agentExecID is ""
	StopSandbox(ctx context.Context, ref string) error                        // graceful path (lifecycle auto-stop)
	// KillSandbox is the kill-switch path: immediate teardown. The control
	// plane cascades identity + credential revocation around this call.
	KillSandbox(ctx context.Context, ref string) error
}

// SandboxEnder is an OPTIONAL Runner capability: stop a sandbox and KEEP it.
// EndSandbox stops the agent without removing it, so files survive, and stops
// the proxy sidecar so nothing the agent could restart has a network path.
// Idempotent. A substrate that cannot keep a stopped sandbox (Kubernetes:
// stopping a pod deletes it) returns ErrEndUnsupported through its router.
type SandboxEnder interface {
	EndSandbox(ctx context.Context, ref string) error
}

// ErrEndUnsupported is EndSandbox's (and StopProxy's) answer from a router
// whose substrate for ref cannot keep a stopped (or lost) sandbox.
var ErrEndUnsupported = errors.New("runner: this substrate cannot keep an ended sandbox")

// ProxyStopper is an OPTIONAL Runner capability: stop a sandbox's proxy
// sidecar and leave its agent running (a run lost to a control-plane outage).
// The stopped proxy is removed, not kept: the config lives in the run's
// stored config (#1176). Idempotent. A graceful stop that fails escalates to
// a kill; a proxy that survives both is an error, with the agent stopped too
// so no work runs while its egress is unconfirmed (#1060).
type ProxyStopper interface {
	StopProxy(ctx context.Context, ref string) error
}

// ProxyReviver is an OPTIONAL Runner capability: replace a sandbox's proxy
// sidecar, running or stopped, with a new one while the agent keeps running.
// The control plane reads the run's stored config (#1176), rewrites its token
// and denies, and hands it to ReplaceProxy.
//
// CanReplaceProxy answers ErrReviveUnsupported when ref's substrate cannot
// replace a proxy in place. ReplaceProxy removes the old proxy, then starts
// the new one at the address the agent's hosts entry pins. An error wrapping
// ErrProxyReplaceFailed means the old proxy is, or may be, gone; any other
// error left it untouched.
type ProxyReviver interface {
	CanReplaceProxy(ctx context.Context, ref string) error
	ReplaceProxy(ctx context.Context, ref string, cfgJSON []byte) error
	// EnsureProxyImage pulls the proxy sidecar image if not already present.
	// Called BEFORE the revive claim so a slow pull doesn't widen the window
	// a watcher sweep must leave the run alone in.
	EnsureProxyImage(ctx context.Context) error
}

// ErrReviveUnsupported is ProxyReviver's answer from a router whose substrate
// for ref cannot replace a proxy in place.
var ErrReviveUnsupported = errors.New("runner: this substrate cannot replace a sandbox's proxy")

// ErrProxyReplaceFailed marks a ReplaceProxy that removed, or may have
// removed, the old proxy and did not start the new one.
var ErrProxyReplaceFailed = errors.New("runner: the old proxy may be gone and the new one did not start")

// SandboxStarter is an OPTIONAL Runner capability: start a kept sandbox's
// stopped agent again (revive after a reboot). Files survive; the main
// process re-runs from the start. Never starts the proxy itself: the caller
// replaces that first (ProxyReviver). Idempotent. Kubernetes (a stopped pod
// is gone) returns ErrReviveUnsupported.
type SandboxStarter interface {
	StartSandbox(ctx context.Context, ref string) error
}

// Freezer is an OPTIONAL Runner capability: pause and resume the AGENT
// container in place. FreezeSandbox pauses the agent process — memory, disk
// and any established TCP connection keep their state — and the daemon
// refuses a new exec until ThawSandbox resumes it. Both idempotent.
//
// The PROXY SIDECAR IS NEVER FROZEN — only the ref this is called with.
//
// A substrate with no pause primitive returns ErrFreezeUnsupported. Implementing
// this is NOT itself proof the pause is safe for every runtime the substrate
// carries: runsc/Kata pause is UNVERIFIED (Capabilities.Freeze[class] is
// false) though Docker implements Freezer. Callers MUST check that field.
type Freezer interface {
	FreezeSandbox(ctx context.Context, ref string) error
	ThawSandbox(ctx context.Context, ref string) error
}

// ErrFreezeUnsupported is Freezer's answer from a router that cannot pause ref.
var ErrFreezeUnsupported = errors.New("runner: this substrate cannot freeze a sandbox")

// ActivitySampler is an OPTIONAL Runner capability: the CPU the agent of each
// sandbox is using, read from the substrate, never by running anything inside
// the sandbox. It is a workload-activity signal and nothing more: a ref with no
// reading is "no reading", neither idle nor gone, and it is never evidence
// that a runner is alive.
//
// SampleCPU returns each ref's agent CPU use in percent of one core. A ref
// missing from the result has no reading. ErrActivityUnavailable means the
// substrate cannot report CPU at all right now (a cluster with no metrics API,
// or one the runner's Role may not read); any other error is a failed read
// that says nothing about whether the signal exists. A nil refs asks only
// whether the signal exists, at the cost of one read.
//
// BatchSample reports that one call reads every sandbox at the cost of one
// read (Kubernetes: one PodMetrics list). When false each ref costs a read of
// its own, and the caller bounds how many it asks for.
type ActivitySampler interface {
	SampleCPU(ctx context.Context, refs []string) (map[string]float64, error)
	BatchSample() bool
}

// ErrActivityUnavailable is ActivitySampler's answer when the substrate has no
// CPU signal to give.
var ErrActivityUnavailable = errors.New("runner: this substrate cannot report sandbox CPU use")

// ImageChecker is an OPTIONAL Runner capability: a substrate whose local image
// cache can go stale (the docker driver) implements this so a stale cache can
// be detected and fallen through to a rebuild. A substrate that pulls fresh
// per launch (k8s) doesn't implement this — callers treat "doesn't implement"
// as unknown, so trust the cache (fail-open).
type ImageChecker interface {
	ImagePresent(ctx context.Context, ref string) (bool, error) // present in the substrate's local image store right now
}

// ImageRemover is an OPTIONAL Runner capability: a substrate with a local
// image store (docker) implements this so a superseded workspace-built image
// can be reclaimed instead of leaking forever. Best-effort, same posture as
// ImageChecker; a substrate that pulls fresh per launch (k8s) doesn't
// implement this.
type ImageRemover interface {
	ImageRemove(ctx context.Context, ref string) error // a ref already absent is NOT an error
}

// DriveProbeResult is the closed set of answers a DriveProber gives about one
// resolved drive mount. Three states, not a bool, because "I checked and it
// is fine" and "I could not tell" are different claims with different
// remedies — see DriveProbeUnknown.
type DriveProbeResult string

const (
	DriveProbeReadable   DriveProbeResult = "readable"   // probe ran AS THE AGENT'S OWN UID (never root) and could read the mount
	DriveProbeUnreadable DriveProbeResult = "unreadable" // agent uid could NOT read it — the failure a daemon-side root os.Stat cannot see
	// DriveProbeUnknown: the probe could not be run to a conclusion (e.g. k8s
	// can only Get the claim's phase). A caller MUST NOT treat Unknown as
	// DriveProbeReadable.
	DriveProbeUnknown DriveProbeResult = "unknown"
)

// DriveProbe is a DriveProber's answer for one resolved mount.
type DriveProbe struct {
	Result DriveProbeResult
	Detail string // operator-facing context; never shown to the member
}

// DriveProber is an OPTIONAL Runner capability, modelled on ImageChecker: a
// substrate that can ask whether the SANDBOX'S OWN USER — not the daemon's
// own root process — can actually read a resolved user-drive mount (a
// daemon-side os.Stat always runs as root and misses this).
type DriveProber interface {
	// ProbeDrive answers whether mount would be readable by the sandbox's
	// uid, bounded by ctx. Called BEFORE a sandbox exists and MUST honour
	// ctx's deadline.
	ProbeDrive(ctx context.Context, mount types.DriveMount) (DriveProbe, error)
}

// SubstrateState is the closed set of answers a SubstrateProber gives about
// whether the control plane can use its substrate right now. Three failure
// classes, not one bool, because the remedies differ: a substrate that cannot
// be reached is a network or daemon fault, a refused credential is a token to
// renew, and a refused verb is a missing grant.
type SubstrateState string

const (
	SubstrateOK           SubstrateState = "ok"
	SubstrateUnreachable  SubstrateState = "unreachable"  // no answer, a transport error, or the probe's deadline
	SubstrateUnauthorized SubstrateState = "unauthorized" // the substrate refused the credential (401)
	SubstrateForbidden    SubstrateState = "forbidden"    // the credential is valid but may not do this (403, a permission error)
)

// SubstrateProber is an OPTIONAL Runner capability, in the shape of
// DriveProber: a cheap read-only call that proves the control plane can reach
// and use its substrate (Kubernetes: one namespaced pod list; Docker: a daemon
// ping). It never creates anything. The implementation classifies its own
// errors, and MUST honour ctx's deadline; a ctx that ended is SubstrateUnreachable.
type SubstrateProber interface {
	ProbeSubstrate(ctx context.Context) SubstrateState
}

// DriveReclaimOutcome is the closed set of answers a DriveReclaimer gives for
// one object it was asked to destroy. The second is not an error: the object
// being gone already is the same END STATE the caller asked for. Telling the
// two apart matters only to the audit row.
type DriveReclaimOutcome string

const (
	DriveReclaimDeleted DriveReclaimOutcome = "deleted" // this call issued the delete and the substrate accepted it
	// DriveReclaimAlreadyAbsent: no object answered to that name. Not an
	// error, but never reported as "deleted" — an operator must be able to
	// trust that row.
	DriveReclaimAlreadyAbsent DriveReclaimOutcome = "already_absent"
)

// ErrDriveInUse is the sentinel a DriveReclaimer returns when a sandbox still
// holds the object; the caller answers 409. A refusal rather than a
// force-delete: a forced remove would pull the volume out from under a live
// agent mid-write.
var ErrDriveInUse = errors.New("runner: the drive's storage is still held by a running sandbox")

// ErrDriveNotReclaimable is the sentinel a DriveReclaimer returns when the
// object answering to the drive's name is NOT the storage it allocated — a
// reclaim that gets this wrong deletes another member's files.
var ErrDriveNotReclaimable = errors.New("runner: the object under this drive's name is not the storage it allocated")

// DriveReclaimer is an OPTIONAL Runner capability, modelled on ImageRemover: a
// substrate that can DESTROY the per-person storage object a user drive
// allocated. The one irreversible verb in this file; a drive OUTLIVES every
// run that mounts it, so no automatic reclaim path may exist. On Kubernetes
// the chart's Role gains `delete` only under `drives.reclaim.enabled`.
type DriveReclaimer interface {
	// ReclaimDrive destroys the storage object mount names, bounded by ctx.
	// MUST refuse rather than destroy when the object is not this drive's
	// (ErrDriveNotReclaimable) or is still held by a sandbox (ErrDriveInUse),
	// and MUST answer DriveReclaimAlreadyAbsent, not an error, when nothing
	// answers to the name.
	ReclaimDrive(ctx context.Context, mount types.DriveMount) (DriveReclaimOutcome, error)
}
