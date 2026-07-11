// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package types defines Wardyn's core domain vocabulary: the four nouns
// (AgentRun, RunPolicy, CredentialGrant, ApprovalRequest) plus the audit
// event shape. These types are the single source of truth shared by the
// control plane, runners, sidecars, and (on Kubernetes) the CRD layer.
package types

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ConfinementClass declares how strongly a sandbox substrate can actually
// confine an agent. Policy may refuse high-trust credential scopes to weak
// classes; the UI must always display the class. See threatmodel/.
type ConfinementClass string

const (
	// CC1: hardened shared-kernel runc (userns, seccomp, AppArmor, cap-drop).
	CC1 ConfinementClass = "CC1"
	// CC2: gVisor userspace kernel (default — runs anywhere Docker runs).
	CC2 ConfinementClass = "CC2"
	// CC3: Kata microVM (requires /dev/kvm).
	CC3 ConfinementClass = "CC3"
)

// Rank orders Confinement Classes weakest→strongest (CC1<CC2<CC3). Unrecognised
// values rank 0 (below CC1) so they never satisfy a real minimum — callers that
// gate on a minimum class fail closed. Matching is exact; normalise the string
// first if the input is untrusted.
func (c ConfinementClass) Rank() int {
	switch c {
	case CC1:
		return 1
	case CC2:
		return 2
	case CC3:
		return 3
	default:
		return 0
	}
}

// RunState is the AgentRun lifecycle state machine.
type RunState string

const (
	RunPending  RunState = "PENDING"
	RunStarting RunState = "STARTING"
	RunRunning  RunState = "RUNNING"
	RunWaiting  RunState = "WAITING_FOR_CONFIRMATION"
	RunStopped  RunState = "STOPPED"
	RunArchived RunState = "ARCHIVED"
	RunFailed   RunState = "FAILED"
	RunKilled   RunState = "KILLED"
	// RunCompleted is the terminal success state: the agent process exited 0.
	// A non-zero exit transitions the run to RunFailed instead. The completion
	// watcher (see internal/api/runs.go dispatch) sets this from RunRunning.
	RunCompleted RunState = "COMPLETED"
)

// ActorType distinguishes who performed an action in the audit stream.
// This is the attribution field the incumbents lack.
type ActorType string

const (
	ActorHuman  ActorType = "human"
	ActorAgent  ActorType = "agent"
	ActorSystem ActorType = "system"
)

// AgentRun is one governed execution of a coding agent on behalf of a human.
// Every run gets its own identity (SPIFFE ID), its own credential grants,
// and its own audit trail.
type AgentRun struct {
	ID               uuid.UUID        `json:"id"`
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
	CreatedBy        string           `json:"created_by"` // human principal (token `sub`)
	Agent            string           `json:"agent"`      // e.g. "claude-code", "codex-cli"
	Repo             string           `json:"repo"`       // e.g. "org/name"
	Task             string           `json:"task"`       // human task description
	PolicyID         *uuid.UUID       `json:"policy_id,omitempty"`
	ConfinementClass ConfinementClass `json:"confinement_class"`
	State            RunState         `json:"state"`
	SPIFFEID         string           `json:"spiffe_id"`     // spiffe://<trust-domain>/agent-run/<id>
	RunnerTarget     string           `json:"runner_target"` // "docker"
	SandboxRef       string           `json:"sandbox_ref,omitempty"`
	// Image is the RESOLVED sandbox image this run dispatched with (convention
	// image, devcontainer build, workspace-built, or BYOI-wrapped), persisted
	// for provenance. Written by a scoped update after image resolution; empty
	// for legacy rows and runs that never reached resolution.
	Image string `json:"image,omitempty"`
	// Interactive marks a run created for human-driven use: the sandbox is brought
	// up RUNNING but no agent task is exec'd and no completion watcher is started
	// (the human drives via `wardyn attach`). A non-interactive run execs the agent
	// with the task and is watched to completion. This is a first-class,
	// sandbox-determining choice — see internal/api dispatch.
	Interactive bool `json:"interactive"`
	// WorkspacePath is the primary host directory this run operates in (the first
	// local WorkspaceMount source resolved from its policy), denormalized here so
	// the control plane can DISCOURAGE — warn, never block — launching a second
	// independent agent against a host directory another active run already uses.
	// Empty for runs with no local host workspace (git-clone / ephemeral).
	WorkspacePath string `json:"workspace_path,omitempty"`
	// WorkspaceID, when set, marks this run as a governed SCAN run for that
	// onboarded workspace: the driver runs wardyn-scan after cloning instead of the
	// agent, and the scan-result endpoint persists the derived profile onto this
	// workspace from this TRUSTED linkage (not sandbox input). Nil for ordinary runs.
	WorkspaceID *uuid.UUID `json:"workspace_id,omitempty"`
}

// RunPolicy is the declarative policy attached to runs: egress allowlist,
// approval rules, and the maximum credential scopes a run may be granted.
// Workspace-local configuration may only NARROW a policy, never widen it.
type RunPolicy struct {
	ID        uuid.UUID     `json:"id"`
	Name      string        `json:"name"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Spec      RunPolicySpec `json:"spec"`
}

// FirstUseMode controls how the egress proxy handles an UNKNOWN domain — one
// that is neither explicitly allowed nor denied — under an allowlist policy. It
// widens the legacy first_use_approval boolean into three explicit modes while
// staying wire-compatible: UnmarshalJSON still accepts the old bool
// (true => deny_with_review, false => always_deny), so existing stored JSONB
// policies decode unchanged. It is inert under allow-all egress.
type FirstUseMode string

const (
	// FirstUseAlwaysDeny hard-denies an unknown domain and logs it, without ever
	// raising it for human approval. (legacy first_use_approval=false)
	FirstUseAlwaysDeny FirstUseMode = "always_deny"
	// FirstUseDenyWithReview raises a pending approval and denies the in-flight
	// request immediately; once a human approves, a later retry passes. The
	// sandbox connection is never held open. (legacy first_use_approval=true)
	FirstUseDenyWithReview FirstUseMode = "deny_with_review"
	// FirstUseWaitForReview raises a pending approval and HOLDS the connection
	// open until it is approved/denied or the proxy's hold deadline passes — the
	// request transparently completes if approved in time. On deadline it fails
	// closed (403) with the approval left pending, degrading to deny_with_review.
	FirstUseWaitForReview FirstUseMode = "wait_for_review"
)

// Normalize maps an empty or unrecognised value to always_deny (fail closed,
// matching the legacy boolean zero value).
func (m FirstUseMode) Normalize() FirstUseMode {
	switch m {
	case FirstUseAlwaysDeny, FirstUseDenyWithReview, FirstUseWaitForReview:
		return m
	default:
		return FirstUseAlwaysDeny
	}
}

// RaisesApproval reports whether an unknown domain is escalated to a human
// rather than hard-denied (true for both review modes).
func (m FirstUseMode) RaisesApproval() bool {
	n := m.Normalize()
	return n == FirstUseDenyWithReview || n == FirstUseWaitForReview
}

// Valid reports whether m is empty (unset => default always_deny) or one of the
// three known modes. Used to reject a hand-authored policy with a garbage value
// at the API boundary; runtime reads still fail closed via Normalize.
func (m FirstUseMode) Valid() bool {
	switch m {
	case "", FirstUseAlwaysDeny, FirstUseDenyWithReview, FirstUseWaitForReview:
		return true
	default:
		return false
	}
}

// UnmarshalJSON accepts BOTH the legacy boolean form (true => deny_with_review,
// false => always_deny) and the new string form, so old stored policies whose
// first_use_approval is a JSON boolean keep decoding without a migration.
func (m *FirstUseMode) UnmarshalJSON(b []byte) error {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || string(t) == "null" {
		*m = FirstUseAlwaysDeny
		return nil
	}
	switch t[0] {
	case 't', 'f': // legacy boolean
		var legacy bool
		if err := json.Unmarshal(t, &legacy); err != nil {
			return err
		}
		if legacy {
			*m = FirstUseDenyWithReview
		} else {
			*m = FirstUseAlwaysDeny
		}
		return nil
	default:
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return err
		}
		// Stored verbatim (not normalized) so validatePolicySpec can reject a
		// garbage value; every runtime read fails closed via Normalize.
		*m = FirstUseMode(s)
		return nil
	}
}

type RunPolicySpec struct {
	// AllowedDomains is the L2 egress allowlist (exact hosts or "*." wildcards).
	AllowedDomains []string `json:"allowed_domains"`
	// DeniedDomains always wins over AllowedDomains.
	DeniedDomains []string `json:"denied_domains,omitempty"`
	// AllowAllEgress switches L2 egress from default-deny (allowlist only) to
	// "allow all (deny-list only)" mode: when true the proxy allows ANY
	// non-denied PUBLIC host, and AllowedDomains may be empty. denied_domains
	// STILL wins. The SSRF/private-IP guard (VetHost/isBlockedIP) is unaffected
	// — allow-all is public hosts only; a host that resolves to a private/
	// loopback/link-local/metadata range is still unconditionally denied. And
	// credential injection STILL requires an EXACT allowlist entry (AllowedExactHost),
	// so allow-all never widens where a secret may be injected — a secret must
	// never leak to an arbitrary host. first_use_approval is inert under allow-all.
	AllowAllEgress bool `json:"allow_all_egress,omitempty"`
	// FirstUseApproval controls how an unknown domain is handled: always_deny
	// (hard-deny, no approval), deny_with_review (raise approval, deny now, retry
	// passes once approved), or wait_for_review (raise approval and hold the
	// connection until decided). Accepts the legacy boolean on the wire. Inert
	// under allow-all.
	FirstUseApproval FirstUseMode `json:"first_use_approval"`
	// AllowedMethods optionally restricts HTTP methods (empty = all).
	AllowedMethods []string `json:"allowed_methods,omitempty"`
	// MinConfinementClass refuses to launch below this class.
	MinConfinementClass ConfinementClass `json:"min_confinement_class"`
	// EligibleGrants is the ceiling of credential scopes a run may request.
	EligibleGrants []GrantSpec `json:"eligible_grants,omitempty"`
	// AutoStopAfter stops idle sandboxes (seconds, 0 = platform default). A
	// NEGATIVE value disables idle reaping ("never reap") — this is what an
	// interactive run (which comes up idle, awaiting a human attach) should use,
	// or the reaper will stop it as soon as it looks idle.
	AutoStopAfterSec int `json:"auto_stop_after_sec,omitempty"`
	// WorkspaceMounts are OPERATOR/ADMIN-controlled host bind mounts injected
	// into the sandbox (e.g. a host repo at ~/work that edits persist to). They
	// are admin-gated: a mount may be authored on a stored policy (via the
	// admin-gated policy CRUD) OR INLINE on a create-run request by an admin /
	// SSO-gated human operator (createRunRequest.InlinePolicy) — both flow
	// through this same RunPolicySpec. A mount is NEVER chosen by the in-sandbox
	// agent: the agent-run entrypoint has no access to either authoring surface,
	// so a prompt-injected agent or a malicious in-sandbox actor can never pick
	// what host paths get mounted. validatePolicySpec runs runner.ValidateMount
	// (the same deny-list the docker driver enforces: absolute cleaned Source not
	// under a dangerous host path; Target under an allowed in-container prefix;
	// default read-only) so a bad mount is rejected at policy-write / inline-
	// validate time (HTTP 400) AND again defense-in-depth in the driver at
	// sandbox-create time.
	WorkspaceMounts []WorkspaceMount `json:"workspace_mounts,omitempty"`
	// WorkspaceRepos are additional git repos attached to a run, paralleling
	// WorkspaceMounts for git-cloned (rather than bind-mounted) sources — the
	// multi-workspace run model (plan core B3). Same admin/inline authoring
	// surface and trust boundary as WorkspaceMounts: never agent-chosen.
	// validatePolicySpec validates each set Target via runner.ValidateTarget and
	// enforces a unique-target invariant across ALL WorkspaceMounts +
	// WorkspaceRepos dests, so a clone can never land on a bind target (or
	// shadow another repo's checkout). Rejecting a repo whose Source is not an
	// ONBOARDED workspace (core B2) is a later, security-critical wave — this
	// type only adds the structural shape.
	WorkspaceRepos []WorkspaceRepo `json:"workspace_repos,omitempty"`
	// LLMInspection optionally enables OUTBOUND content inspection at the proxy
	// for brokered LLM routes (the "inadvertent-leak guardrail"). Nil/omitted =>
	// OFF (no scanning) — the safe default, mirroring WorkspaceMount.ReadOnly's
	// pointer-means-unset idiom. It is a guardrail + visibility layer, NOT
	// exfiltration prevention (see internal/contentscan + threatmodel §5.1).
	LLMInspection *LLMInspectionSpec `json:"llm_inspection,omitempty"`
	// Resources caps sandbox CPU/memory/PIDs/disk. Nil, or a zero field, means
	// "use the platform default": the dispatch path fills conservative defaults so
	// EVERY run is capped even when a policy sets nothing. These are the basic
	// multi-tenant safety controls that let a FLEET of independent agents coexist —
	// without them one runaway or prompt-injected agent can OOM-kill the host,
	// fork-bomb the host PID space, or fill host storage and take down sibling runs.
	Resources *ResourceLimits `json:"resources,omitempty"`
}

// LLMInspectionSpec configures optional outbound LLM prompt inspection for a
// run. The zero value (or a nil *LLMInspectionSpec on the policy) means OFF.
//
// v1 ships only the known-secret detector (exact match against operator-declared
// WorkspaceSecretValues); DetectEntropy/DetectPII are reserved for later phases.
type LLMInspectionSpec struct {
	// Mode is "off" (default), "alert" (scan + audit, forward unchanged), or
	// "block" (a qualifying finding refuses the request). "" == "off".
	Mode string `json:"mode"`
	// WorkspaceSecretValues are operator-declared known secret VALUES (e.g. the
	// contents of a mounted .env) that the run must not leak into a prompt. They
	// are the v1 detection corpus and are NEVER logged. Values shorter than the
	// masking floor are ignored.
	WorkspaceSecretValues []string `json:"workspace_secret_values,omitempty"`
	// DetectSecrets enables the known-secret detector (exact match against
	// operator-declared WorkspaceSecretValues). At least one Detect* must be true
	// when Mode != off.
	DetectSecrets bool `json:"detect_secrets,omitempty"`
	// DetectSecretPatterns enables the regex catalog of well-known secret FORMATS
	// (AWS/GitHub/Slack/Google keys, PEM private keys, JWTs, Stripe). Higher
	// precision than entropy but can false-positive on example/test keys in code.
	DetectSecretPatterns bool `json:"detect_secret_patterns,omitempty"`
	// DetectEntropy enables the Shannon-entropy detector (high FP in code; medium
	// severity so a strict block_min_severity can exclude it). Off by default.
	DetectEntropy bool `json:"detect_entropy,omitempty"`
	// DetectPII enables the regex/Luhn PII detector (best-effort, high false-
	// negative recall; a visibility signal, never a control). Off by default.
	DetectPII bool `json:"detect_pii,omitempty"`
	// DetectorSidecarURL, when set, adds an out-of-process detection sidecar
	// (e.g. a Presidio / Protect-AI LLM-Guard wrapper) the proxy POSTs each span
	// to. Trusted operator config (not agent-chosen). A sidecar error/timeout/non-200
	// is treated as a scanner error and respects on_scanner_error like any other:
	// fail-OPEN by default (the request still flows), fail-CLOSED (the request is
	// blocked in block mode) when on_scanner_error=block — so block mode with the
	// sidecar as the SOLE detector DOES guarantee a block on sidecar failure.
	DetectorSidecarURL string `json:"detector_sidecar_url,omitempty"`
	// ClassifiedMarkers are operator-defined literal markers (e.g. "INTERNAL ONLY",
	// "CONFIDENTIAL//NOFORN") whose presence in outbound content flags a
	// classified-content leak (the "proprietary content shouldn't leave" sense of
	// the walled garden). Case-insensitive substring match; category "classified".
	ClassifiedMarkers []string `json:"classified_markers,omitempty"`
	// ScanAttachments opts into decoding+scanning base64 image/document attachment
	// bytes in a prompt (off by default — binary, large, high-FP).
	ScanAttachments bool `json:"scan_attachments,omitempty"`
	// InspectForwardEgress extends inspection from the LLM routes to the GENERIC
	// plaintext-HTTP forward path, so a custom (non-LLM) HTTP connector's POST/PUT
	// body is scanned too. Off by default. (HTTPS connectors tunnel via opaque
	// CONNECT and remain uninspected unless MITM'd — see threatmodel §5.1a.)
	InspectForwardEgress bool `json:"inspect_forward_egress,omitempty"`
	// MaxScanBytes caps the size of a single extracted span scanned; 0 => a
	// built-in default. A larger span is skipped (fail-open) and recorded.
	MaxScanBytes int `json:"max_scan_bytes,omitempty"`
	// OnScannerError selects behavior when the scanner ERRORS (e.g. an
	// unparseable body): "pass" (default, fail-open) or "block".
	OnScannerError string `json:"on_scanner_error,omitempty"`
	// RequireInspectableLLM, when true, refuses to schedule a run whose resolved
	// LLM transport is opaque (subscription-OAuth/Bedrock CONNECT) and therefore
	// cannot be inspected — fail-closed, like MinConfinementClass. Default false
	// only WARNS (the common subscription user is not punished).
	RequireInspectableLLM bool `json:"require_inspectable_llm,omitempty"`
	// InterceptTLS opts the run into TLS-MITM of opaque CONNECT tunnels to known
	// LLM hosts (Anthropic/OpenAI), making the subscription-OAuth path inspectable.
	// The control plane provisions a per-run CA: the PRIVATE key goes only to the
	// proxy sidecar; the sandbox trusts only the CA's PUBLIC cert. Adds a CA trust
	// dependency inside the sandbox — see threatmodel §5.1a. Off by default.
	InterceptTLS bool `json:"intercept_tls,omitempty"`
	// BlockMinSeverity is the minimum finding severity that triggers a block in
	// "block" mode ("low"|"medium"|"high"|"critical"; "" => low).
	BlockMinSeverity string `json:"block_min_severity,omitempty"`
}

// WorkspaceMount is one operator/policy-controlled host bind mount. Source is a
// host path (must be an absolute, cleaned path not under a denied location);
// Target is the in-container path (must be under an allowed prefix, e.g.
// /home/agent, /work, or /workspace).
//
// ReadOnly is a *bool so the SAFE DEFAULT is read-only: when the field is OMITTED
// (nil) the mount is mounted read-only. Read-write requires the policy author to
// EXPLICITLY set "read_only": false. (A plain bool would default to false ==
// read-write, the unsafe direction; the pointer makes "unset" mean read-only.)
// Use ReadOnlyOrDefault to resolve the effective value.
type WorkspaceMount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly *bool  `json:"read_only,omitempty"`
}

// ReadOnlyOrDefault resolves the mount's effective read-only flag: an OMITTED
// (nil) read_only defaults to true (read-only), the safe default. Read-write is
// returned only when the policy explicitly set read_only=false.
func (m WorkspaceMount) ReadOnlyOrDefault() bool {
	if m.ReadOnly == nil {
		return true
	}
	return *m.ReadOnly
}

// WorkspaceRepo is one operator/policy-controlled git repo attached to a run,
// paralleling WorkspaceMount for git-cloned (rather than bind-mounted)
// sources (plan core B3, multi-workspace run model). Repo is a slug/URL
// validated the same way the legacy single-repo AgentRun.Repo field is
// (repoFieldSafe + repoCloneURL, runs.go). Target is an optional in-container
// clone destination; an empty Target defers to the ~/work/<name> convention a
// later wave wires up (plan B4, WARDYN_REPOS) — it is NOT resolved here.
type WorkspaceRepo struct {
	Repo   string `json:"repo"`
	Target string `json:"target,omitempty"`
}

// WorkspaceKind discriminates an onboarded Workspace's source shape.
type WorkspaceKind string

const (
	WorkspaceKindLocalDir WorkspaceKind = "local_dir"
	WorkspaceKindRepo     WorkspaceKind = "repo"
)

// WorkspaceStatus is the onboarding/scan lifecycle of a Workspace.
type WorkspaceStatus string

const (
	// WorkspacePendingScan is the initial state: onboarded but not yet scanned.
	WorkspacePendingScan WorkspaceStatus = "pending_scan"
	// WorkspaceScanning: a scan run is in flight (repo scan).
	WorkspaceScanning WorkspaceStatus = "scanning"
	// WorkspaceScanned: profile derived; the operator is configuring the import.
	WorkspaceScanned WorkspaceStatus = "scanned"
	// WorkspaceBuilding: the environment image is being generated/built.
	WorkspaceBuilding WorkspaceStatus = "building"
	// WorkspaceBuildError: the environment image build failed.
	WorkspaceBuildError WorkspaceStatus = "build_error"
	// WorkspaceVerifying: a verify run is executing install/build/test.
	WorkspaceVerifying WorkspaceStatus = "verifying"
	// WorkspaceVerifyFailed: the last verify run's setup commands failed.
	WorkspaceVerifyFailed WorkspaceStatus = "verify_failed"
	// WorkspaceReady means the workspace is scanned, built, and (when a runner
	// is available) verified — Profile/ImageRef populated and current.
	WorkspaceReady WorkspaceStatus = "ready"
	// WorkspaceError means the last scan attempt failed.
	WorkspaceError WorkspaceStatus = "error"
)

// Workspace is an onboarded, admin-reviewed local dir or repo a run may
// attach (plan core B1). Import scans/reviews the source ONCE and persists a
// profile; runs thereafter reference the workspace by id instead of a
// free-text host path or repo slug. Repos are re-cloned fresh per run but
// reuse the scan-once Profile.
//
// Profile is core A's WorkspaceProfile (internal/workspacescan) serialized
// opaquely: core B (this type) never interprets it, only persists/returns it
// — core A owns the shape and BuiltProfileHash cache-keying. Both are empty
// until Status transitions out of pending_scan.
type Workspace struct {
	ID   uuid.UUID     `json:"id"`
	Name string        `json:"name"`
	Kind WorkspaceKind `json:"kind"`
	// Source is a host path for Kind=local_dir, or a repo slug/URL for Kind=repo.
	Source string `json:"source"`
	// Ref is an optional git ref (branch/tag/sha); repo kind only.
	Ref string `json:"ref,omitempty"`
	// DefaultTarget is the in-container mount/clone path a run uses absent a
	// per-attach override (WorkspaceMount.Target / WorkspaceRepo.Target).
	DefaultTarget string `json:"default_target,omitempty"`
	// Profile is core A's WorkspaceProfile, opaque here. Nil/empty until scanned.
	Profile json.RawMessage `json:"profile,omitempty"`
	// ImageRef is the resolved/generated image for this workspace's profile
	// (core A, A5); empty until scanned/built.
	ImageRef string `json:"image_ref,omitempty"`
	// BuiltProfileHash is the profile hash ImageRef was built from — core A's
	// build-once/reuse-many cache key (rebuild only when the profile hash changes).
	BuiltProfileHash string `json:"built_profile_hash,omitempty"`
	// ApprovedEgress is the OPERATOR-owned list of egress hosts explicitly
	// promoted for this workspace (typically from the scanner's content-derived
	// SuggestedEgress, which is advisory and never auto-allowed). Unioned into a
	// run's allowlist alongside the scanned profile's EgressDomains. Never
	// written by a scan; cleared when source/kind changes.
	ApprovedEgress []string `json:"approved_egress,omitempty"`
	// SetupCommands is the OPERATOR-approved list of install/build/test/lint
	// commands the import runs to VERIFY the environment. Promoted from the
	// scanner's advisory profile.SetupCommands (never auto-run — a detected
	// command executes only after this explicit approval, and only in a
	// confinement sandbox). Opaque []workspacescan.SetupCommand JSON; cleared on
	// source/kind change. Mirrors ApprovedEgress's operator-owned discipline.
	SetupCommands json.RawMessage `json:"setup_commands,omitempty"`
	// VerifyResult is the last verify run's per-step outcome (exit codes +
	// rolling head+tail logs), opaque []workspacescan-style JSON, re-derived
	// control-plane-side from the run's upload. Nil until a verify run reports.
	VerifyResult json.RawMessage `json:"verify_result,omitempty"`
	// VerifiedProfileHash / VerifiedAt record that ImageRef was PROVEN to
	// install/build/test successfully (distinct from BuiltProfileHash, which only
	// means "built"). Empty until a green verify.
	VerifiedProfileHash string     `json:"verified_profile_hash,omitempty"`
	VerifiedAt          *time.Time `json:"verified_at,omitempty"`
	// ActiveRunID is the in-flight scan/build/verify/record run for this
	// workspace, so the import panel can poll "is my step still running" without
	// scanning all runs. Nil when no import step is executing.
	ActiveRunID *uuid.UUID `json:"active_run_id,omitempty"`
	// RecordResults is the per-task Record Mode state (taskKey → result: run
	// pointer, mode, status, observations captured server-side from the run's
	// audit events). Opaque map[string]api-owned JSON, like VerifyResult; written
	// only via the scoped SetWorkspaceRecordResults; cleared on source/kind
	// change (recordings were reviewed against the OLD source).
	RecordResults json.RawMessage `json:"record_results,omitempty"`
	Status        WorkspaceStatus `json:"status"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// ResourceLimits caps a sandbox's resource consumption. A ZERO field means "use
// the platform default": the dispatch path fills conservative defaults (e.g.
// 2000m CPU, 4096 MiB, 512 PIDs) so even a policy that sets no limits still runs
// capped. DiskMiB is best-effort and depends on the storage driver supporting a
// per-container quota (fail-closed/warn when a cap is demanded but unsupported).
type ResourceLimits struct {
	CPUMillis int `json:"cpu_millis,omitempty"` // milli-CPU; 2000 = 2 vCPU
	MemoryMiB int `json:"memory_mib,omitempty"` // hard memory cap in MiB
	PidsLimit int `json:"pids_limit,omitempty"` // max processes/threads (fork-bomb guard)
	DiskMiB   int `json:"disk_mib,omitempty"`   // writable storage cap in MiB (best-effort)
}

// SiteConfig is the operator-wide, admin-authored baseline every run inherits:
// a corporate upstream proxy, per-ecosystem artifact-registry overrides, and
// default SCM hosts. It is the ONE net-new persistence surface the enterprise
// Getting-Started enhancements introduce (the Host Proxy and Artifact
// Repository Redirection steps read it; everything else rides secrets +
// grants). There is exactly one SiteConfig for the operator (a store
// singleton); GetSiteConfig returns the zero value when none has been written
// yet — "unconfigured" is a valid, common state, not an error.
//
// Secret VALUES never live here — only secret NAMES (refs) the broker/proxy
// resolve at dispatch/injection time, mirroring how RunPolicySpec's
// GrantSpec.Scope references secrets by name rather than embedding them.
type SiteConfig struct {
	// UpstreamProxySecretRef names a secret holding the corporate upstream proxy
	// URL (optionally with embedded user:pass), or "" when no upstream proxy is
	// configured.
	UpstreamProxySecretRef string `json:"upstream_proxy_secret_ref,omitempty"`
	// ArtifactOverrides maps an ecosystem ("npm"|"pip"|"cargo"|"maven"|"go"|
	// "nuget") to its corporate artifact-registry redirect. A missing key means
	// that ecosystem is unconfigured (public registry, untouched).
	ArtifactOverrides map[string]ArtifactOverride `json:"artifact_overrides,omitempty"`
	// ScmHosts are the operator's default SCM hosts (e.g. "dev.azure.com",
	// "github.example.com") the SCM Provider step / egress bundling consult.
	ScmHosts []string `json:"scm_hosts,omitempty"`
}

// ArtifactOverride is one ecosystem's corporate artifact-registry redirect: the
// base URL to emit into that ecosystem's config (.npmrc/pip.conf/cargo config/
// settings.xml/GOPROXY/nuget.config) plus an optional secret ref for a token
// injected proxy-side (the sandbox never holds the value).
type ArtifactOverride struct {
	BaseURL        string `json:"base_url"`
	TokenSecretRef string `json:"token_secret_ref,omitempty"`
}

// GrantKind enumerates broker-mintable credential kinds.
type GrantKind string

const (
	GrantGitHubToken GrantKind = "github_token"
	GrantCloudSTS    GrantKind = "cloud_sts" // HARD-REQUIRES SPIRE identity provider
	GrantAPIKey      GrantKind = "api_key"   // proxy-side injection only
	// GrantGitPAT returns a STORED Personal Access Token VALUE to the git
	// credential helper (username/password) for a matched non-GitHub git host
	// (Azure DevOps / GitLab). This is the OPPOSITE of api_key: git-over-HTTPS to
	// those hosts is an opaque CONNECT tunnel the proxy cannot inject Basic-auth
	// into, so the credential must reach git via the helper — like github_token.
	GrantGitPAT GrantKind = "git_pat"
	// GrantSSHKey materializes a RESIDENT, agent-readable SSH private key so a run
	// can clone git-over-SSH. It is a DOCUMENTED EXCEPTION to the no-resident-
	// secret invariant: git's SSH transport has NO credential-helper seam (git
	// credential.helper is HTTP-only), so neither the git_pat helper trick nor
	// api_key proxy-side injection can carry the key — it MUST land as a 0400 file
	// the ssh client reads. agent-run writes it just before the clone and wipes it
	// right after (see deploy/images/*/agent-run), so the readable window is the
	// clone only. Transport is SSH-over-443 (ssh.github.com / ssh.dev.azure.com)
	// through the wardyn-proxy CONNECT tunnel — no port-22 egress. Residual risk is
	// the same posture as WARDYN_GIT_HELPER_SECRET: code running AS the agent uid
	// can read the key during that window. See broker.mintSSHKey + threat model.
	GrantSSHKey GrantKind = "ssh_key"
)

// SubscriptionOAuthSecret is a SENTINEL secret name (NOT a stored secret). An
// api_key injection grant carrying it resolves at inject time to the operator's
// LIVE Anthropic subscription OAuth token (from the resident ~/.claude), not a
// value in the secret store — so subscription runs are credentialed proxy-side
// like api-key runs, the sandbox holding only the inert sentinel. It also serves
// as the durable "this profile uses subscription LLM auth" marker on a recorded
// profile (the resident ~/.claude mount that would otherwise signal subscription
// is never synthesized). Shared here so api + recordmode + UI agree on the name.
const SubscriptionOAuthSecret = "anthropic-subscription-oauth"

// GrantSpec is a credential scope description. The broker enforces the
// invariant: a minted credential's scope is exactly the approved scope —
// never wider (no scope-widening between request and mint).
type GrantSpec struct {
	Kind GrantKind `json:"kind"`
	// Scope is kind-specific: for github_token {"repos":[...],"permissions":{...}},
	// for api_key {"host":"...","header":"..."}, for git_pat
	// {"host":"...","secret_name":"...","username":"<optional>"} — the stored
	// secret_name's PAT value is returned to the git credential helper as the
	// password for host, with username resolved by convention (ADO=pat,
	// GitLab=oauth2) unless overridden — and for ssh_key
	// {"host":"...","key_secret_ref":"...","username":"<optional, default git>",
	// "known_hosts_secret_ref":"<optional>"} — the stored key_secret_ref's private
	// key VALUE is returned to agent-run, which writes it to a 0400 file for the
	// SSH-over-443 clone and wipes it after (see GrantSSHKey).
	Scope json.RawMessage `json:"scope"`
	// TTL of the minted credential. Max (and default) 1h.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// RequiresApproval forces a human approval to mint (vs auto-mint on policy).
	RequiresApproval bool `json:"requires_approval"`
}

// CredentialGrant records what a run is ELIGIBLE for. Eligibility is not
// issuance: minting happens only via the broker, and for RequiresApproval
// grants only inside the same DB transaction that verifies an APPROVED
// ApprovalRequest for this run+scope.
type CredentialGrant struct {
	ID        uuid.UUID `json:"id"`
	RunID     uuid.UUID `json:"run_id"`
	CreatedAt time.Time `json:"created_at"`
	Spec      GrantSpec `json:"spec"`
}

// ApprovalKind enumerates what a human is being asked to approve.
type ApprovalKind string

const (
	ApprovalCredential   ApprovalKind = "credential"
	ApprovalEgressDomain ApprovalKind = "egress_domain"
	ApprovalToolCall     ApprovalKind = "tool_call"
)

// ApprovalState is the approval lifecycle.
type ApprovalState string

const (
	ApprovalPending  ApprovalState = "PENDING"
	ApprovalApproved ApprovalState = "APPROVED"
	ApprovalDenied   ApprovalState = "DENIED"
	ApprovalExpired  ApprovalState = "EXPIRED"
)

// ApprovalRequest is a blocking human-in-the-loop gate. RequestedScope is
// EXACTLY what the approver saw; the broker writes MintedJTI back in the
// same transaction as the mint, yielding the provable join
// "approval X by human Y minted credential Z".
type ApprovalRequest struct {
	ID             uuid.UUID       `json:"id"`
	RunID          uuid.UUID       `json:"run_id"`
	GrantID        *uuid.UUID      `json:"grant_id,omitempty"`
	Kind           ApprovalKind    `json:"kind"`
	RequestedScope json.RawMessage `json:"requested_scope"`
	State          ApprovalState   `json:"state"`
	RequestedAt    time.Time       `json:"requested_at"`
	DecidedAt      *time.Time      `json:"decided_at,omitempty"`
	DecidedBy      string          `json:"decided_by,omitempty"`
	MintedJTI      string          `json:"minted_jti,omitempty"`
	Reason         string          `json:"reason,omitempty"`
}

// AuditEvent is one append-only audit record. Every credential mint/revoke,
// approval decision, policy change, egress decision, and lifecycle change
// emits one. Events carry the delegation chain (human sub + agent run).
//
// Action namespaces (the dotted-verb prefix discriminates the source stream):
//
//	credential.*  identity.*  approval.*  policy.*  egress.*  run.* recording.*
//	    — control-plane / agent self-report events (Postgres event log + PTY).
//	kernel.*      — the eBPF/Tetragon GROUND-TRUTH stream (the tamper-proof
//	                second stream). Emitted ONLY by the host-scoped sensor
//	                (cmd/wardyn-tetragon-ingest) via POST /api/v1/internal/
//	                groundtruth, which FORCES actor_type=system +
//	                actor="wardyn-tetragon-ingest" and rejects any action that
//	                does not carry the "kernel." prefix. The defined kernel.*
//	                actions are:
//	                  kernel.process.exec    — observed execve
//	                  kernel.network.connect — observed outbound TCP connect
//	                  kernel.file.write      — observed write to a sensitive path
//	                  kernel.sensor.heartbeat— sensor liveness (run_id NULL)
//	                  kernel.sensor.blind    — host eBPF blind to a run (CC3/Kata)
//
// Data shape for the kernel.* (ebpf) stream. audit_events.data is JSONB, so
// this requires NO schema change — it is a documented convention over the
// existing column. Every kernel.* event carries:
//
//	{
//	  "stream": "ebpf",                 // discriminates from agent self-report
//	  "subtype": "process_exec" | "network_connect" | "file_write" | ...,
//	  "cgroup_id": <uint64>,            // kernel cgroup id (omitempty)
//	  "container_id": "<id>",           // attributed container (omitempty)
//	  "argv": [...] | "dst": "ip:port" | "path": "/...",  // kind-specific
//	  "loader": true,                   // exec of a dynamic linker (ld-linux/
//	                                    // ld-musl) — the documented ld-linux/
//	                                    // mmap bypass surface, FLAGGED not blocked
//	  "correlation": "mapped" | "unmapped",  // unmapped => run_id NULL, never
//	                                          // silently dropped (visible blindness)
//	  "reason": "...",                  // sensor.blind / failure detail (omitempty)
//	  "dropped_total": <uint64>         // heartbeat only: sensor backpressure drops
//	}
//
// Outcome stays within the existing CHECK ("success"|"failure"|"denied"): the
// kernel stream uses "success" for normal observations and "failure" for the
// unexpected — an unmapped/escape signal such as a connect to a private/
// link-local/metadata (non-proxy) address, or a sensor.blind coverage gap. It
// never uses "denied": this is a DETECTION stream, it does not block. See
// internal/groundtruth for the canonical action/data definitions.
type AuditEvent struct {
	ID        uuid.UUID       `json:"id"`
	Time      time.Time       `json:"time"`
	RunID     *uuid.UUID      `json:"run_id,omitempty"`
	ActorType ActorType       `json:"actor_type"`
	Actor     string          `json:"actor"`  // human sub, agent SPIFFE ID, or component name
	Action    string          `json:"action"` // dotted verb, e.g. "credential.mint", "egress.deny", "kernel.process.exec"
	Target    string          `json:"target,omitempty"`
	Outcome   string          `json:"outcome"` // "success" | "failure" | "denied"
	SourceIP  string          `json:"source_ip,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}
