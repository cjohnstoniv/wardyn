// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// policy.go carries the RUN POLICY seam — RunPolicy plus every shape its spec
// is made of: the unknown-domain FirstUseMode, the optional outbound
// LLM-inspection config, the mounts and repos a run's workspace is composed
// from, and the sandbox resource caps. Split out of types.go when that file
// crossed the size gate; RunPolicy is still one of the four nouns the package
// doc names, so the doc is left as written (it describes the PACKAGE).

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
// that is neither explicitly allowed nor denied — under an allowlist policy.
// It widens the legacy first_use_approval boolean into three explicit modes
// while staying wire-compatible: UnmarshalJSON still accepts the old bool
// (true => deny_with_review, false => always_deny). Inert under allow-all egress.
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
	// AllowAllEgress switches L2 egress from default-deny to "allow all
	// (deny-list only)" mode: the proxy allows ANY non-denied PUBLIC host, and
	// AllowedDomains may be empty. denied_domains STILL wins; the SSRF/private-IP
	// guard is unaffected; credential injection STILL requires an EXACT
	// allowlist entry, so allow-all never widens where a secret may be
	// injected. first_use_approval is inert under allow-all.
	AllowAllEgress bool `json:"allow_all_egress,omitempty"`
	// FirstUseApproval controls how an unknown domain is handled: always_deny,
	// deny_with_review, or wait_for_review. Accepts the legacy boolean on the
	// wire. Inert under allow-all.
	FirstUseApproval FirstUseMode `json:"first_use_approval"`
	// FirstUseHoldSeconds bounds how long a wait_for_review connection is HELD
	// open awaiting a decision before the proxy refuses it. 0/absent keeps the
	// built-in 30s default; a positive value overrides it. Only wait_for_review
	// holds. See internal/egress/proxy configureHold.
	FirstUseHoldSeconds int `json:"first_use_hold_seconds,omitempty"`
	// MaxHolds caps concurrent wait_for_review holds. 0/absent keeps the
	// built-in 16 default. The (N+1)th concurrent held connection fails fast
	// rather than consuming an unbounded goroutine.
	MaxHolds int `json:"max_holds,omitempty"`
	// AllowedMethods optionally restricts HTTP methods (empty = all).
	AllowedMethods []string `json:"allowed_methods,omitempty"`
	// MinConfinementClass refuses to launch below this class.
	MinConfinementClass ConfinementClass `json:"min_confinement_class"`
	// EligibleGrants is the ceiling of credential scopes a run may request.
	EligibleGrants []GrantSpec `json:"eligible_grants,omitempty"`
	// AutoStopAfter stops idle sandboxes after this many seconds. 0 (and an
	// ABSENT field) means never reaped, identical to an explicit negative (the
	// reaper skips <= 0); negative exists only to state never-reap intent
	// explicitly.
	AutoStopAfterSec int `json:"auto_stop_after_sec,omitempty"`
	// WorkspaceMounts are OPERATOR/ADMIN-controlled host bind mounts injected
	// into the sandbox. Admin-gated: authored on a stored policy OR INLINE on a
	// create-run request by an admin/SSO-gated operator — both flow through this
	// same spec. NEVER chosen by the in-sandbox agent (no authoring surface
	// reaches it). validatePolicySpec runs runner.ValidateMount (the same
	// deny-list the docker driver enforces) so a bad mount is rejected at
	// write/inline-validate time AND again defense-in-depth at sandbox-create.
	WorkspaceMounts []WorkspaceMount `json:"workspace_mounts,omitempty"`
	// WorkspaceRepos are additional git repos attached to a run, paralleling
	// WorkspaceMounts for git-cloned (rather than bind-mounted) sources — the
	// multi-workspace run model. Same admin/inline authoring surface and trust
	// boundary as WorkspaceMounts: never agent-chosen. validatePolicySpec
	// enforces a unique-target invariant across ALL WorkspaceMounts +
	// WorkspaceRepos dests, so a clone can never land on a bind target or
	// shadow another repo's checkout. Rejecting a repo whose Source is not an
	// ONBOARDED workspace is a later wave — this type only adds the shape.
	WorkspaceRepos []WorkspaceRepo `json:"workspace_repos,omitempty"`
	// LLMInspection optionally enables OUTBOUND content inspection at the proxy
	// for brokered LLM routes. Nil/omitted => OFF (the safe default). A
	// guardrail + visibility layer, NOT exfiltration prevention (see
	// internal/contentscan + threatmodel §5.1).
	LLMInspection *LLMInspectionSpec `json:"llm_inspection,omitempty"`
	// UIApps declares the in-sandbox loopback HTTP apps the UI gateway may relay
	// to a browser. OPERATOR-AUTHORED, same trust boundary as WorkspaceMounts:
	// an app is a NAME, a loopback PORT and a PATH — never a command string.
	// The gateway serves ONLY a declared port; `ssh -L` stays the
	// undeclared-port escape hatch. Empty = no UI apps.
	UIApps []UIApp `json:"ui_apps,omitempty"`
	// Resources caps sandbox CPU/memory/PIDs/disk. Nil, or a zero field, means
	// "use the platform default" — dispatch fills conservative defaults so
	// EVERY run is capped, the multi-tenant safety controls that keep one
	// runaway/prompt-injected agent from OOM-killing the host, fork-bombing it,
	// or filling storage and taking down sibling runs.
	Resources *ResourceLimits `json:"resources,omitempty"`
	// ToolRules narrows an autonomous run's tool use from a BINARY to a policy.
	//
	// `tool_approvals` alone has exactly two settings: `auto` (no gate at all)
	// and `hold` (every gated call to a human) — one is more autonomy than an
	// operator wants, the other more interruptions than a human answers, which
	// is how approval fatigue starts. A rule names a TOOL and an effect (allow/
	// hold/deny), so "read freely, ask before shell, never fetch the web"
	// becomes expressible.
	//
	// OPERATOR-AUTHORED and evaluated OUTSIDE the sandbox — proxy-side — so a
	// compromised agent cannot rewrite its own rules. Empty preserves today's
	// exact behaviour: every gated call under `hold` goes to a human.
	//
	// It narrows; it never widens. Consulted ONLY for a run already in `hold`,
	// so a bad rule can at worst ask a human more often or refuse a call.
	ToolRules []ToolRule `json:"tool_rules,omitempty"`
	// GitPushAnyBranch turns OFF branch-namespace confinement for THIS run's
	// brokered pushes — on the GitHub App lane (on by default) and the git_pat
	// lane where the operator opted in (the pat scope of
	// WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS, off by default). One field, both brokers. By default the proxy git-broker
	// forwards a push only when every ref lives under
	// refs/heads/wardyn/<run-id>/, so an agent can't rewrite main — right for
	// an autonomous run, wrong for a sandbox a HUMAN drives through an external
	// tool that names its own branches.
	//
	// true = forward pushes to any branch the granted token may write. Audited
	// per push (rule_source brokered:git:branch-ns-off); the grant's own GitHub
	// ruleset still bounds what the token can touch. Operator-authored, never
	// agent-settable; false/absent keeps today's behaviour exactly.
	//
	// ponytail: whole-namespace off, not a per-run allowed-prefix list — add a
	// prefix list when someone needs an external tool AND confinement at once.
	GitPushAnyBranch bool `json:"git_push_any_branch,omitempty"`
	// PushRules declares CONTENT rules for this run's brokered git pushes — WHAT
	// a push may touch, alongside GitPushAnyBranch's WHERE. Nil (every policy
	// authored before this field existed) means no content rules, byte-identical
	// to that prior wire shape and behaviour, since every reader keys off IsSet.
	//
	// A policy that sets PushRules while this run's only git-capable grant is
	// ssh_key is legal but UNENFORCEABLE (the SSH transport has no broker seam)
	// — composer.Grade surfaces that as a medium-risk WARNING, never a
	// write-time refusal.
	PushRules *PushRulesSpec `json:"push_rules,omitempty"`
}

// PushRulesSpec declares content rules for a run's brokered git pushes — the
// counterpart to GitPushAnyBranch's branch-namespace confinement: WHAT a push
// may touch, not WHERE it may land (issue #57).
//
// DenyNewExecutables and MaxFileSizeMiB are reserved for a later change.
//
// A closed struct, deliberately not a free-form rules map: an open map cannot
// be policed by the strict-field JSON decoder or the doc-census test, so a
// typo'd key would silently do nothing instead of failing at write time.
//
// This type stores and VALIDATES; how DenyPathSegments entries MATCH is the
// git broker's (internal/egress/proxy/push_rules.go), which reads them on
// both brokered lanes.
type PushRulesSpec struct {
	// DenyPaths are path patterns (e.g. ".github/workflows/**") the broker
	// refuses in a push: "**" crosses path segments, "*" and "?" do not, and
	// the pattern is anchored at the repository root. A trailing "/" means
	// everything beneath the directory (DenyPathSegments).
	DenyPaths []string `json:"deny_paths,omitempty"`
	// MaxInspectPackMiB caps how much of an incoming push the broker buffers
	// before refusing it as too large. 0/absent takes the broker's own default,
	// below the authored maximum so raising this is a real remedy; bounded
	// 0..64 by validatePolicySpec.
	MaxInspectPackMiB int `json:"max_inspect_pack_mib,omitempty"`
	// RequireReviewPaths are patterns in DenyPaths' language whose match HOLDS
	// a push for an admin's decision instead of refusing it. A deny match wins
	// over a review match; an unattended run refuses rather than holding.
	RequireReviewPaths []string `json:"require_review_paths,omitempty"`
	// HoldSeconds is how long a held push waits for that decision before it
	// is refused. 0/absent means 120; bounded 0..600 by validatePolicySpec.
	HoldSeconds int `json:"hold_seconds,omitempty"`
}

// IsSet reports whether this spec carries an actual rule — NOT a bare != nil.
// An all-zero-but-non-nil *PushRulesSpec (a literal `push_rules: {}`) says
// nothing about what a push may touch and must read exactly like an absent
// one wherever consulted (composer's clamp/risk grade, the broker's
// advertisement/enforcement) — one method so those readers can't drift.
func (s *PushRulesSpec) IsSet() bool {
	return s != nil && (len(s.DenyPaths) > 0 || len(s.RequireReviewPaths) > 0 || s.MaxInspectPackMiB > 0)
}

// DenyPathSegments is the one reading of a push_rules.deny_paths entry, shared
// by write-time validation and the broker's matcher so the two cannot disagree.
//
// A leading "/" is dropped: patterns are anchored at the root either way. A
// trailing "/" means everything beneath (the .gitignore/CODEOWNERS spelling),
// so "infra/" reads as "infra/**". An empty, "." or ".." segment is refused —
// git never stores such a path, so it would silently match nothing, which
// reads as enforcement that isn't there. Invalid UTF-8 and leading/trailing
// whitespace are refused too (a near-certain typo).
func DenyPathSegments(pattern string) ([]string, error) {
	if !utf8.ValidString(pattern) {
		return nil, fmt.Errorf("%q is not valid UTF-8", pattern)
	}
	p := strings.TrimPrefix(pattern, "/")
	if strings.TrimSpace(p) != p {
		return nil, fmt.Errorf("%q has leading or trailing whitespace (almost certainly a typo)", pattern)
	}
	if strings.HasSuffix(p, "/") {
		p += "**"
	}
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return nil, fmt.Errorf("%q has an empty, \".\" or \"..\" path segment, which no git path contains", pattern)
		}
	}
	return segs, nil
}

// ToolEffect is what a matching ToolRule does with the call.
type ToolEffect string

const (
	// ToolAllow runs the call with no human in the loop. It is still recorded.
	ToolAllow ToolEffect = "allow"
	// ToolHold raises an approval and blocks — today's behaviour for every call.
	ToolHold ToolEffect = "hold"
	// ToolDeny refuses the call without asking anyone.
	ToolDeny ToolEffect = "deny"
)

// ValidToolEffect reports whether e is one of the three effects. A closed enum,
// checked at every ingest point: an unrecognised effect must be a 400, never a
// silently-ignored rule that reads as enforcement.
func ValidToolEffect(e ToolEffect) bool {
	switch e {
	case ToolAllow, ToolHold, ToolDeny:
		return true
	}
	return false
}

// ToolRule is one entry in ToolRules: a tool NAME and what to do with it.
//
// Tool is matched EXACTLY and case-sensitively against the name the harness
// reports (e.g. "Read", "Bash", "WebFetch"). Deliberately not a pattern: a
// glob over tool names invites a rule that reads narrower than it matches, and
// the set of tools a harness exposes is small and enumerable. The one
// wildcard is the literal "*", which sets the default for unmatched tools.
type ToolRule struct {
	Tool   string     `json:"tool"`
	Effect ToolEffect `json:"effect"`
}

// Clone returns a deep copy: every slice/pointer field is reallocated, so the
// copy shares NO backing array with the receiver.
//
// A plain `spec := other` is a SHALLOW copy — slice headers still point at
// the original's backing array, making the process-global default policy
// aliasable: a per-run `append` to AllowedDomains wrote into the shared spare
// capacity, so two concurrent create-runs raced on the same element (one
// run's egress domain silently replacing another's), and any in-place
// mutation leaked into every later run. Callers that derive a per-run/
// per-request spec from a shared one MUST Clone first.
func (s RunPolicySpec) Clone() RunPolicySpec {
	out := s // shallow: copies the scalars; slice/pointer fields fixed up below.
	out.AllowedDomains = append([]string(nil), s.AllowedDomains...)
	out.DeniedDomains = append([]string(nil), s.DeniedDomains...)
	out.AllowedMethods = append([]string(nil), s.AllowedMethods...)
	out.EligibleGrants = append([]GrantSpec(nil), s.EligibleGrants...)
	out.WorkspaceMounts = append([]WorkspaceMount(nil), s.WorkspaceMounts...)
	out.WorkspaceRepos = append([]WorkspaceRepo(nil), s.WorkspaceRepos...)
	out.UIApps = append([]UIApp(nil), s.UIApps...)
	out.ToolRules = append([]ToolRule(nil), s.ToolRules...)
	if s.LLMInspection != nil {
		li := *s.LLMInspection
		// Deep-copy the nested slice fields too, or this "clone" still aliases
		// the receiver's backing arrays through the copied pointer.
		li.WorkspaceSecretNames = append([]string(nil), s.LLMInspection.WorkspaceSecretNames...)
		li.WorkspaceSecretValues = append([]string(nil), s.LLMInspection.WorkspaceSecretValues...)
		li.ClassifiedMarkers = append([]string(nil), s.LLMInspection.ClassifiedMarkers...)
		out.LLMInspection = &li
	}
	if s.Resources != nil {
		r := *s.Resources
		out.Resources = &r
	}
	if s.PushRules != nil {
		pr := *s.PushRules
		pr.DenyPaths = append([]string(nil), s.PushRules.DenyPaths...)
		pr.RequireReviewPaths = append([]string(nil), s.PushRules.RequireReviewPaths...)
		out.PushRules = &pr
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
	// the at-rest secret store — the field an operator/admin actually AUTHORS.
	// A name is not sensitive the way a value is, so it may freely appear in a
	// stored policy row or read. Resolved to WorkspaceSecretValues ONLY at
	// dispatch, in memory, on the per-dispatch policy copy handed to the proxy.
	WorkspaceSecretNames []string `json:"workspace_secret_names,omitempty"`
	// WorkspaceSecretValues are the RESOLVED secret VALUES the run must not leak
	// into a prompt — the v1 detection corpus the proxy actually matches
	// against. NOT an authoring field: validatePolicySpec refuses a non-empty
	// value here on every policy write. Populated ONLY by dispatch, in memory;
	// every other copy carries names only, values redacted to a count. NEVER
	// logged. Values shorter than the masking floor are ignored.
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
	// DetectorSidecarURL, when set, adds an out-of-process detection sidecar the
	// proxy POSTs each span to. Trusted operator config. A sidecar error/
	// timeout/non-200 respects on_scanner_error like any other scanner error:
	// fail-OPEN by default, fail-CLOSED when on_scanner_error=block — so block
	// mode with the sidecar as the SOLE detector DOES guarantee a block on
	// sidecar failure.
	DetectorSidecarURL string `json:"detector_sidecar_url,omitempty"`
	// ClassifiedMarkers are operator-defined literal markers (e.g. "INTERNAL ONLY",
	// "CONFIDENTIAL//NOFORN") whose presence in outbound content flags a
	// classified-content leak. Case-insensitive substring match; category "classified".
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
	// LLM hosts (Anthropic/OpenAI), making the subscription-OAuth path
	// inspectable. The control plane provisions a per-run CA: the PRIVATE key
	// goes only to the proxy sidecar; the sandbox trusts only the CA's PUBLIC
	// cert. Adds a CA trust dependency inside the sandbox — see threatmodel
	// §5.1a. Off by default.
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
// ReadOnly is a *bool so the SAFE DEFAULT is read-only: when the field is
// OMITTED (nil) the mount is read-only. Read-write requires the policy author
// to EXPLICITLY set "read_only": false (a plain bool would default to the
// unsafe read-write direction). Use ReadOnlyOrDefault to resolve the value.
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
// paralleling WorkspaceMount for git-cloned (rather than bind-mounted) sources
// (the multi-workspace run model). Repo is a slug/URL validated the same way
// the legacy single-repo AgentRun.Repo field is. Target is an optional
// in-container clone destination; an empty Target defers to the ~/work/<name>
// convention a later wave wires up — NOT resolved here. Ref is an optional
// git ref (branch/tag/sha); empty clones the remote's default branch. Carried
// as a 4th tab-separated field in WARDYN_REPOS for agent-run-lib.sh's clone_one.
type WorkspaceRepo struct {
	Repo   string `json:"repo"`
	Target string `json:"target,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

// UIApp is one declared in-sandbox HTTP app the UI gateway may relay to
// (RunPolicySpec.UIApps). Three fields, no command string:
//
//   - Name identifies the app to the operator and the launcher convention
//     (/usr/local/bin/wardyn-ui-<name>), constrained to a short lower-case
//     slug — it reaches a filesystem path and a URL query, and neither may
//     ever see a slash, a space or "..".
//   - Port is the port the app listens on INSIDE the sandbox, on 127.0.0.1.
//     The sandbox has no other reachable address (invariant 3).
//   - Path is where the app's UI lives, the landing path after the gateway's
//     ticket handoff. Empty means "/".
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
