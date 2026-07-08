// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package workspacescan deterministically detects a local directory's (or a
// cloned repo's) development conventions — languages, package managers,
// implied egress registries, dev-container/Dockerfile presence, tools, and
// git remotes — so Wardyn can onboard a workspace with a profile grounded in
// what's ACTUALLY in the tree, not an LLM guess.
//
// It clones internal/gitremote's conventions: read-only, bounded
// filepath.WalkDir (depth<=4), a manifest-count cap and a 1 MiB per-file read
// cap, NO symlink following, a control-char scrub on any string that crosses
// out of the scan, sorted+deduped output, NO subprocess/exec, and
// fail-safe-to-empty — Scan and DeriveProfile never return an error; a scan
// that hits a bound or an unrecognized build system just yields a
// lower-confidence profile, never a crash or a grant on uncertainty.
//
// Two data shapes (A2, isolation-critical split):
//   - ScanFacts is raw bounded evidence a scan emits. When it comes from a
//     sandboxed repo scan (governed run, a later wave) it crosses the sandbox
//     boundary and MUST be treated as untrusted.
//   - WorkspaceProfile is the validated authority the control plane derives
//     (DeriveProfile) and persists. Egress hosts in a WorkspaceProfile ONLY
//     ever come from the fixed markers.go table, keyed on filenames — NEVER
//     from file contents — so a hostile manifest body can't inject a host.
package workspacescan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Confidence buckets how much a WorkspaceProfile can be trusted without a
// human/AI review pass.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

// Source records how a WorkspaceProfile was derived. Wave 1 only ever
// produces SourceDeterministic; SourceAIAssisted is reserved for the (later,
// not-this-wave) AI fallback pass.
const (
	SourceDeterministic = "deterministic"
	SourceAIAssisted    = "ai_assisted"
)

// GitRemotes mirrors internal/gitremote.DetectGitHubRepos's (github,
// otherHosts) return shape as a named, JSON-friendly struct: the sorted
// "owner/repo" GitHub remotes found, and the sorted set of non-GitHub remote
// HOSTS (for an operator warning / git_pat grant grounding).
type GitRemotes struct {
	GitHub     []string `json:"github,omitempty"`
	OtherHosts []string `json:"other_hosts,omitempty"`
}

// ManifestHit is one recognized marker file found during a scan.
type ManifestHit struct {
	Path   string `json:"path"`   // slash-separated, relative to the scan root
	Marker string `json:"marker"` // canonical marker id, e.g. "package-lock.json"
}

// SecretNeed is one secret/config key a workspace's committed files REFERENCE
// BY NAME — never a value. Detectors (detect.go) capture only the identifier
// left of the '='/':' delimiter or inside a ${...} placeholder; the rest of
// the line is discarded before anything is stored. Optional means the file
// declared a safe default (Spring `${VAR:default}`), a commented template
// line, or a deploy-time key (SealedSecret) — surfaced for information, never
// a launch blocker. Kind is a coarse env-name-family classification
// ("postgres", "oidc", "deploy", ... — see classifySecretKind); "generic"
// when unknown.
type SecretNeed struct {
	Name     string `json:"name"`
	Kind     string `json:"kind,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// SetupCommand is one conventional environment-setup step a workspace implies:
// install dependencies, build, test, or lint. SECURITY: the Command string is
// NEVER copied from a file's content (a hostile package.json `scripts.build`
// could be `rm -rf`); it is synthesized from a FIXED template keyed on the
// detected package manager + which conventional script/target KEYS exist —
// exactly the filename-keyed discipline egress hosts use. Advisory only: a
// command is surfaced for operator review and only ever executed inside a
// confinement sandbox after explicit approval (mirrors SuggestedEgress).
type SetupCommand struct {
	Stage   string `json:"stage"`   // install | build | test | lint
	Command string `json:"command"` // fixed-template command, never file content
	Source  string `json:"source"`  // what implied it, e.g. "convention:go", "package.json:build"
}

// LeakFinding is a CONTENT-FREE report of a suspected committed secret VALUE.
// The leaked-value detector (detect.go) is the ONE lane that reads file values
// — to recognize a secret-shaped token — but it stores only WHERE and WHAT
// KIND, never the matched bytes: Kind is a detector id ("aws-access-key",
// "github-pat", ...), never the value. This mirrors internal/contentscan's
// content-free Finding discipline.
type LeakFinding struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Line int    `json:"line,omitempty"`
}

// UnrecognizedSample is a bounded, scrubbed snippet of a file that looked
// like a build/dependency descriptor but isn't in the fixed marker table —
// evidence for the (later) AI fallback. Content is truncated and has control
// characters stripped; it is never large enough, nor selected in a way, to
// leak a secret value.
type UnrecognizedSample struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ScanFacts is the raw bounded evidence a scan emits. It is untrusted input
// to DeriveProfile: a WorkspaceProfile is always re-derived from these facts
// control-plane-side, never taken on faith from whatever produced them.
type ScanFacts struct {
	ManifestsFound      []ManifestHit        `json:"manifests_found,omitempty"`
	GitRemotes          GitRemotes           `json:"git_remotes,omitempty"`
	HasDevcontainer     bool                 `json:"has_devcontainer,omitempty"`
	HasDockerfile       bool                 `json:"has_dockerfile,omitempty"`
	UnrecognizedSamples []UnrecognizedSample `json:"unrecognized_samples,omitempty"`
	Truncated           bool                 `json:"truncated,omitempty"`

	// Content-lane evidence (detect.go): names/keys/hosts only, extracted via
	// anchored capture groups — no file value ever lands here. Like every other
	// fact these are UNTRUSTED until DeriveProfile re-validates and caps them.
	SecretRequirements []SecretNeed  `json:"secret_requirements,omitempty"`
	ServicesFound      []string      `json:"services_found,omitempty"`
	SuggestedEgress    []string      `json:"suggested_egress,omitempty"`
	SecretFilesPresent []string      `json:"secret_files_present,omitempty"`
	BuildMemoryMiB     int           `json:"build_memory_mib,omitempty"`
	LeakFindings       []LeakFinding `json:"leak_findings,omitempty"`
	// Raw setup-command SIGNALS (not commands): which conventional script/target
	// keys exist. DeriveProfile synthesizes fixed-template SetupCommands from
	// these + the detected package managers — file content never becomes a command.
	ScriptKeys  []string `json:"script_keys,omitempty"`  // package.json scripts: build|test|lint present
	MakeTargets []string `json:"make_targets,omitempty"` // Makefile targets: build|test|install|lint present
	// BuildInputHashes maps a build-input file's rel path to a hex sha256 of its
	// CONTENT (devcontainer.json / Dockerfile). A digest, not content — safe.
	BuildInputHashes map[string]string `json:"build_input_hashes,omitempty"`
}

// WorkspaceProfile is the validated, control-plane-owned authority derived
// from a scan. Every slice field is sorted + deduped. It is safe to persist
// and to hand to run-creation for egress/grant/image decisions (A6, a later
// wave).
type WorkspaceProfile struct {
	Languages       []string   `json:"languages,omitempty"`
	PackageManagers []string   `json:"package_managers,omitempty"`
	EgressDomains   []string   `json:"egress_domains,omitempty"`
	Tools           []string   `json:"tools,omitempty"`
	GitRemotes      GitRemotes `json:"git_remotes,omitempty"`
	HasDevcontainer bool       `json:"has_devcontainer,omitempty"`
	HasDockerfile   bool       `json:"has_dockerfile,omitempty"`

	// Advisory "needs" fields (content lane, validated by DeriveProfile).
	// RequiredSecrets/ServicesNeeded/SecretFilesPresent inform the operator
	// (needs panel, setup checklist) and never gate a launch or create a
	// grant. SuggestedEgress is content-derived and is NEVER auto-unioned
	// into a run's allowlist (that privilege is EgressDomains-only, which
	// stays filename-keyed) — an operator promotes hosts into the
	// workspace's operator-owned ApprovedEgress list instead.
	// ponytail: these advisory fields ride in ProfileHash, so a workspace's
	// first rescan after this change forces one no-op image rebuild. Ceiling:
	// harmless one-time churn; upgrade path — hash only the image-affecting
	// subset (Languages/HasDevcontainer/HasDockerfile) if it ever bites.
	RequiredSecrets    []SecretNeed `json:"required_secrets,omitempty"`
	ServicesNeeded     []string     `json:"services_needed,omitempty"`
	SuggestedEgress    []string     `json:"suggested_egress,omitempty"`
	SecretFilesPresent []string     `json:"secret_files_present,omitempty"`
	// BuildMemoryMiB is the largest build-heap ceiling detected (JVM -Xmx /
	// Node --max-old-space-size). Advisory: surfaced so an operator can size
	// the sandbox; never auto-applied to a run's ResourceLimits.
	BuildMemoryMiB int `json:"build_memory_mib,omitempty"`
	// LeakFindings are content-free reports of suspected committed secret
	// values (path + detector kind + line, never the value). Advisory warning.
	LeakFindings []LeakFinding `json:"leak_findings,omitempty"`
	// SetupCommands are the conventional install/build/test/lint steps this
	// workspace implies, synthesized from fixed templates (never file content).
	// Advisory: operator-approved before they ever run, and only in a sandbox.
	SetupCommands []SetupCommand `json:"setup_commands,omitempty"`
	// ContextHash is a digest of the BUILD-INPUT files' CONTENT (a repo's own
	// devcontainer.json / Dockerfile). It rides ProfileHash so the built-image
	// cache (BuiltProfileHash) busts when a build input changes even if the
	// detected profile is otherwise identical — the gap a profile-only hash has
	// for the repo-owns-its-devcontainer build path. Empty when no build-input
	// files are present (generated-devcontainer path is already profile-derived).
	ContextHash string `json:"context_hash,omitempty"`
	// Confidence is one of ConfidenceHigh/Medium/Low.
	Confidence  string `json:"confidence"`
	NeedsReview bool   `json:"needs_review,omitempty"`
	// Source is one of SourceDeterministic/SourceAIAssisted.
	Source string `json:"source"`
}

// ProfileHash returns the SHA-256 hex digest of the profile's canonical
// (sorted-object-keys) JSON form. It's used to cache-key generated/built
// images (Workspace.BuiltProfileHash, a later wave): the same detected
// profile always hashes the same, regardless of Go struct field order.
//
// encoding/json marshals a map[string]any with its keys sorted, so a
// marshal → unmarshal-into-map → marshal round trip is a standard-library-only
// way to get canonical JSON without hand-rolling a key sort.
func (p WorkspaceProfile) ProfileHash() string {
	b, err := json.Marshal(p)
	if err != nil {
		return "" // unreachable for this struct; fail-safe rather than panic
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	canon, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}
