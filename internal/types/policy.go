// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// policy.go carries the RUN POLICY seam — RunPolicy plus every shape its spec
// is made of: the unknown-domain FirstUseMode, the optional outbound
// LLM-inspection config, the mounts and repos a run's workspace is composed
// from, and the sandbox resource caps. Split out of types.go when that file
// crossed the size gate, exactly as workspace.go was, and nothing here changed
// in the move. Unlike that split, this one moves core vocabulary: RunPolicy is
// one of the four nouns the package doc names, so a reader following that doc
// to types.go will not find it there. The doc is left as written because it
// describes the PACKAGE, which still defines all four — not any one file.

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
	// FirstUseHoldSeconds bounds how long a wait_for_review connection is HELD
	// open awaiting a decision before the proxy refuses it. 0/absent keeps the
	// built-in 30s default (back-compat); a positive value overrides it. Only
	// wait_for_review holds; the other first-use modes never hold. See
	// internal/egress/proxy configureHold.
	FirstUseHoldSeconds int `json:"first_use_hold_seconds,omitempty"`
	// MaxHolds caps concurrent wait_for_review holds (one held goroutine per
	// held connection). 0/absent keeps the built-in 16 default; a positive value
	// overrides it. The (N+1)th concurrent held connection fails fast rather than
	// consuming an unbounded goroutine.
	MaxHolds int `json:"max_holds,omitempty"`
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
	// multi-workspace run model. Same admin/inline authoring
	// surface and trust boundary as WorkspaceMounts: never agent-chosen.
	// validatePolicySpec validates each set Target via runner.ValidateTarget and
	// enforces a unique-target invariant across ALL WorkspaceMounts +
	// WorkspaceRepos dests, so a clone can never land on a bind target (or
	// shadow another repo's checkout). Rejecting a repo whose Source is not an
	// ONBOARDED workspace is a later, security-critical wave — this
	// type only adds the structural shape.
	WorkspaceRepos []WorkspaceRepo `json:"workspace_repos,omitempty"`
	// LLMInspection optionally enables OUTBOUND content inspection at the proxy
	// for brokered LLM routes (the "inadvertent-leak guardrail"). Nil/omitted =>
	// OFF (no scanning) — the safe default, mirroring WorkspaceMount.ReadOnly's
	// pointer-means-unset idiom. It is a guardrail + visibility layer, NOT
	// exfiltration prevention (see internal/contentscan + threatmodel §5.1).
	LLMInspection *LLMInspectionSpec `json:"llm_inspection,omitempty"`
	// UIApps declares the in-sandbox loopback HTTP apps the UI gateway may relay
	// to a browser (a code editor, a dev server). OPERATOR-AUTHORED, the same
	// trust boundary as WorkspaceMounts: an app is a NAME, a loopback PORT and a
	// PATH — never a command string, so a policy can never choose what runs
	// inside the sandbox (the launcher is a convention,
	// /usr/local/bin/wardyn-ui-<name>). The gateway serves ONLY a declared port
	// and refuses anything else naming this field; `ssh -L` stays the
	// undeclared-port escape hatch. Empty (the default) = this run has no UI
	// apps, which is what every policy authored before this field said.
	UIApps []UIApp `json:"ui_apps,omitempty"`
	// Resources caps sandbox CPU/memory/PIDs/disk. Nil, or a zero field, means
	// "use the platform default": the dispatch path fills conservative defaults so
	// EVERY run is capped even when a policy sets nothing. These are the basic
	// multi-tenant safety controls that let a FLEET of independent agents coexist —
	// without them one runaway or prompt-injected agent can OOM-kill the host,
	// fork-bomb the host PID space, or fill host storage and take down sibling runs.
	Resources *ResourceLimits `json:"resources,omitempty"`
}

// Clone returns a deep copy: every slice/pointer field is reallocated, so the
// copy shares NO backing array with the receiver.
//
// A plain `spec := other` is a SHALLOW copy — the struct is duplicated but each
// slice header still points at the original's backing array. That made the
// process-global default policy aliasable: a per-run `append` to AllowedDomains
// wrote into the shared spare capacity, so two concurrent create-runs raced on
// the same element (one run's egress domain silently replacing another's in the
// allowlist handed to its proxy), and any in-place mutation leaked into every
// later run. Callers that derive a per-run/per-request spec from a shared one
// MUST Clone first.
func (s RunPolicySpec) Clone() RunPolicySpec {
	out := s // shallow: copies the scalars; slice/pointer fields fixed up below.
	out.AllowedDomains = append([]string(nil), s.AllowedDomains...)
	out.DeniedDomains = append([]string(nil), s.DeniedDomains...)
	out.AllowedMethods = append([]string(nil), s.AllowedMethods...)
	out.EligibleGrants = append([]GrantSpec(nil), s.EligibleGrants...)
	out.WorkspaceMounts = append([]WorkspaceMount(nil), s.WorkspaceMounts...)
	out.WorkspaceRepos = append([]WorkspaceRepo(nil), s.WorkspaceRepos...)
	out.UIApps = append([]UIApp(nil), s.UIApps...)
	if s.LLMInspection != nil {
		li := *s.LLMInspection
		// Deep-copy the nested slice fields too, or this "clone" still aliases
		// the receiver's backing arrays through the copied pointer — exactly the
		// hazard this whole method exists to prevent (see the doc comment above).
		li.WorkspaceSecretNames = append([]string(nil), s.LLMInspection.WorkspaceSecretNames...)
		li.WorkspaceSecretValues = append([]string(nil), s.LLMInspection.WorkspaceSecretValues...)
		li.ClassifiedMarkers = append([]string(nil), s.LLMInspection.ClassifiedMarkers...)
		out.LLMInspection = &li
	}
	if s.Resources != nil {
		r := *s.Resources
		out.Resources = &r
	}
	return out
}

// LLMInspectionSpec configures optional outbound LLM prompt inspection for a
// run. The zero value (or a nil *LLMInspectionSpec on the policy) means OFF.
type LLMInspectionSpec struct {
	// Mode is "off" (default), "alert" (scan + audit, forward unchanged), or
	// "block" (a qualifying finding refuses the request). "" == "off".
	Mode string `json:"mode"`
	// WorkspaceSecretNames are operator-declared secret NAMES, resolved against
	// the at-rest secret store — the field an operator/admin actually AUTHORS
	// (on a stored policy or an inline_policy). This is the storable form of the
	// detection corpus: a name is not sensitive the way a value is, so it may
	// freely appear in a stored policy row, a policy read, or a compose/profile
	// proposal returned to a caller. Resolved to WorkspaceSecretValues ONLY at
	// dispatch (resolveLLMInspectionSecrets, internal/api/runs_dispatch.go), in
	// memory, on the per-dispatch policy copy handed to the proxy sidecar.
	WorkspaceSecretNames []string `json:"workspace_secret_names,omitempty"`
	// WorkspaceSecretValues are the RESOLVED secret VALUES (e.g. the contents of
	// a mounted .env) the run must not leak into a prompt — the v1 detection
	// corpus the proxy sidecar actually matches against. NOT an authoring
	// field: validatePolicySpec refuses a non-empty value here on every policy
	// write (stored, inline, or WARDYN_DEFAULT_POLICY) — author
	// WorkspaceSecretNames instead. Populated ONLY by dispatch, in memory, on
	// the ephemeral copy of the spec sent to the proxy; every other copy (the
	// stored row, a policy read DTO, a compose/profile proposal, the
	// run.policy.effective audit event) carries names only, values redacted to
	// a count. NEVER logged. Values shorter than the masking floor are ignored.
	WorkspaceSecretValues []string `json:"workspace_secret_values,omitempty"`
	// DetectSecrets enables the known-secret detector (exact match against the
	// resolved WorkspaceSecretValues corpus). At least one Detect* must be true
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
// sources (the multi-workspace run model). Repo is a slug/URL
// validated the same way the legacy single-repo AgentRun.Repo field is
// (repoFieldSafe + repoCloneURL, runs.go). Target is an optional in-container
// clone destination; an empty Target defers to the ~/work/<name> convention a
// later wave wires up (plan B4, WARDYN_REPOS) — it is NOT resolved here.
// Ref is an optional git ref (branch/tag/sha) — same field WorkspaceSource
// carries as part of a repo source's identity (workspace.go); an empty Ref
// clones the remote's default branch, unchanged from before this field
// existed. Carried as a 4th tab-separated field in WARDYN_REPOS
// (buildRepoRecords, runs_scm.go) for agent-run-lib.sh's clone_one to check
// out (W9-S1-3 — previously advertised on the source's identity but never
// actually honored by any clone).
type WorkspaceRepo struct {
	Repo   string `json:"repo"`
	Target string `json:"target,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

// UIApp is one declared in-sandbox HTTP app the UI gateway may relay to
// (RunPolicySpec.UIApps). Three fields, no command string:
//
//   - Name identifies the app to the operator and to the launcher convention
//     (/usr/local/bin/wardyn-ui-<name>), so it is constrained to a short
//     lower-case slug — it reaches a filesystem path and a URL query, and
//     neither may ever see a slash, a space or "..".
//   - Port is the port the app listens on INSIDE the sandbox, on 127.0.0.1.
//     The sandbox has no other reachable address (invariant 3) and the relay
//     dials nothing else.
//   - Path is where the app's UI lives, used as the landing path after the
//     gateway's ticket handoff. Empty means "/".
type UIApp struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	Path string `json:"path,omitempty"`
}

// PathOrRoot is Path with the empty default applied.
func (a UIApp) PathOrRoot() string {
	if a.Path == "" {
		return "/"
	}
	return a.Path
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
