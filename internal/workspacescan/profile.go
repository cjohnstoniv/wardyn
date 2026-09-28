// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package workspacescan deterministically detects a workspace's dev
// conventions (languages, package managers, egress registries,
// devcontainer/Dockerfile presence, tools, git remotes) from what's actually
// in the tree, not an LLM guess.
//
// It follows internal/gitremote's conventions: read-only, bounded
// filepath.WalkDir (depth<=6), a manifest-count cap, a 1 MiB per-file read
// cap, no symlink following, control-char scrubbing, sorted+deduped output,
// no subprocess/exec, and fail-safe-to-empty — Scan and DeriveProfile never
// return an error; hitting a bound or an unrecognized build system just
// yields a lower-confidence profile, never a crash or a grant on uncertainty.
//
// Two data shapes, isolation-critical:
//   - ScanFacts is raw bounded evidence a scan emits; once it crosses a
//     sandbox boundary it MUST be treated as untrusted.
//   - WorkspaceProfile is the validated authority the control plane derives
//     (DeriveProfile) and persists. SECURITY: its egress hosts come ONLY from
//     the fixed markers.go table keyed on filenames — NEVER from file
//     contents — so a hostile manifest body can't inject a host.
package workspacescan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
)

// Confidence buckets how much a WorkspaceProfile can be trusted without a
// human/AI review pass.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

// Source records how a WorkspaceProfile was derived. Only SourceDeterministic
// ships today; SourceAIAssisted is reserved for a later AI fallback pass.
const (
	SourceDeterministic = "deterministic"
	SourceAIAssisted    = "ai_assisted"
)

// GitRemotes mirrors gitremote.DetectGitHubRepos's return shape: sorted
// "owner/repo" GitHub remotes and sorted non-GitHub hosts (for an operator
// warning / git_pat grant grounding).
type GitRemotes struct {
	GitHub     []string `json:"github,omitempty"`
	OtherHosts []string `json:"other_hosts,omitempty"`
}

// ManifestHit is one recognized marker file found during a scan.
type ManifestHit struct {
	Path   string `json:"path"`   // slash-separated, relative to the scan root
	Marker string `json:"marker"` // canonical marker id, e.g. "package-lock.json"
}

// SecretNeed is one secret/config key a workspace's committed files
// REFERENCE BY NAME — never a value. Detectors capture only the identifier
// before '='/':' or inside a ${...} placeholder; the rest of the line is
// discarded before anything is stored. Optional marks a safe default,
// commented template line, or deploy-time key — informational, never a
// launch blocker. Kind is a coarse family classification; "generic" when
// unknown.
type SecretNeed struct {
	Name     string `json:"name"`
	Kind     string `json:"kind,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// SetupCommand is one conventional environment-setup step a workspace
// implies (install/build/test/lint). SECURITY: Command is NEVER copied from
// file content (a hostile package.json `scripts.build` could be `rm -rf`) —
// it's synthesized from a FIXED template keyed on the detected package
// manager and which conventional script/target KEYS exist. Advisory only:
// surfaced for operator review, executed only inside a confinement sandbox
// after explicit approval.
type SetupCommand struct {
	Stage   string `json:"stage"`   // install | build | test | lint
	Command string `json:"command"` // fixed-template command, never file content
	Source  string `json:"source"`  // what implied it, e.g. "convention:go", "package.json:build"
}

// LeakFinding is a CONTENT-FREE report of a suspected committed secret VALUE.
// The leaked-value detector is the ONE lane that reads file values (to
// recognize a secret-shaped token), but it stores only WHERE and WHAT KIND —
// Kind is a detector id, never the matched bytes.
type LeakFinding struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	Line int    `json:"line,omitempty"`
	// Source names the attached source once multiple sources merge into one
	// profile; empty otherwise. A separate field rather than a Path prefix,
	// because Path is path-classified downstream (testdata|__tests__|fixtures)
	// and folding a locator in would misclassify leaks under a source whose
	// own path looks like a fixture.
	Source string `json:"source,omitempty"`
}

// UnrecognizedSample is a bounded, scrubbed snippet of a file that looked
// like a build/dependency descriptor but isn't in the fixed marker table —
// evidence for a later AI fallback. Content is truncated and control-char
// stripped, never large enough to leak a secret value.
type UnrecognizedSample struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// ScanFacts is the raw bounded evidence a scan emits — untrusted input to
// DeriveProfile, which always re-derives a WorkspaceProfile from these facts
// rather than trusting the producer.
type ScanFacts struct {
	ManifestsFound      []ManifestHit        `json:"manifests_found,omitempty"`
	GitRemotes          GitRemotes           `json:"git_remotes,omitempty"`
	HasDevcontainer     bool                 `json:"has_devcontainer,omitempty"`
	HasDockerfile       bool                 `json:"has_dockerfile,omitempty"`
	UnrecognizedSamples []UnrecognizedSample `json:"unrecognized_samples,omitempty"`
	Truncated           bool                 `json:"truncated,omitempty"`

	// Content-lane evidence: names/keys/hosts only, via anchored capture
	// groups — no file value ever lands here. UNTRUSTED until DeriveProfile
	// re-validates and caps it.
	SecretRequirements []SecretNeed  `json:"secret_requirements,omitempty"`
	ServicesFound      []string      `json:"services_found,omitempty"`
	SuggestedEgress    []string      `json:"suggested_egress,omitempty"`
	SecretFilesPresent []string      `json:"secret_files_present,omitempty"`
	BuildMemoryMiB     int           `json:"build_memory_mib,omitempty"`
	LeakFindings       []LeakFinding `json:"leak_findings,omitempty"`
	// Raw setup-command SIGNALS (not commands): which conventional script/target
	// keys exist. DeriveProfile synthesizes fixed-template SetupCommands from
	// these plus detected package managers — file content never becomes a command.
	ScriptKeys  []string `json:"script_keys,omitempty"`  // package.json scripts: build|test|lint present
	MakeTargets []string `json:"make_targets,omitempty"` // Makefile targets: build|test|install|lint present
	// BuildInputHashes maps a build-input file's path to a hex sha256 of its
	// content (devcontainer.json / Dockerfile) — a digest, not content, so safe.
	BuildInputHashes map[string]string `json:"build_input_hashes,omitempty"`
}

// WorkspaceProfile is the validated, control-plane-owned authority derived
// from a scan. Every slice field is sorted + deduped; safe to persist and
// hand to run-creation for egress/grant/image decisions.
type WorkspaceProfile struct {
	Languages       []string `json:"languages,omitempty"`
	PackageManagers []string `json:"package_managers,omitempty"`
	// ToolchainNeeds below reads these two: dispatch env derives from what
	// the scan actually detected, never a platform-wide guess.
	EgressDomains   []string   `json:"egress_domains,omitempty"`
	Tools           []string   `json:"tools,omitempty"`
	GitRemotes      GitRemotes `json:"git_remotes,omitempty"`
	HasDevcontainer bool       `json:"has_devcontainer,omitempty"`
	HasDockerfile   bool       `json:"has_dockerfile,omitempty"`

	// Advisory "needs" fields (content lane, validated by DeriveProfile)
	// inform the operator and never gate a launch or create a grant.
	// SECURITY: SuggestedEgress is content-derived and is NEVER auto-unioned
	// into a run's allowlist — only EgressDomains (filename-keyed) grants
	// that; an operator must promote hosts into ApprovedEgress explicitly.
	// These fields ride in ProfileHash, so a rescan after this changes
	// forces one harmless no-op image rebuild; hash only the image-affecting
	// subset instead if that churn ever bites.
	RequiredSecrets    []SecretNeed `json:"required_secrets,omitempty"`
	ServicesNeeded     []string     `json:"services_needed,omitempty"`
	SuggestedEgress    []string     `json:"suggested_egress,omitempty"`
	SecretFilesPresent []string     `json:"secret_files_present,omitempty"`
	// BuildMemoryMiB is the largest build-heap ceiling detected (JVM -Xmx /
	// Node --max-old-space-size). Advisory sizing hint only, never
	// auto-applied to a run's ResourceLimits.
	BuildMemoryMiB int `json:"build_memory_mib,omitempty"`
	// LeakFindings are content-free reports of suspected committed secret
	// values (path + detector kind + line, never the value). Advisory warning.
	LeakFindings []LeakFinding `json:"leak_findings,omitempty"`
	// SetupCommands are the conventional install/build/test/lint steps
	// implied, synthesized from fixed templates only. Advisory:
	// operator-approved, sandbox-only execution.
	SetupCommands []SetupCommand `json:"setup_commands,omitempty"`
	// ContextHash digests the build-input files' content (devcontainer.json /
	// Dockerfile); it rides ProfileHash so the built-image cache busts when a
	// build input changes even if the detected profile is unchanged. Empty
	// when no build-input files are present.
	ContextHash string `json:"context_hash,omitempty"`
	// Confidence is one of ConfidenceHigh/Medium/Low.
	Confidence  string `json:"confidence"`
	NeedsReview bool   `json:"needs_review,omitempty"`
	// Source is one of SourceDeterministic/SourceAIAssisted.
	Source string `json:"source"`
}

// ProfileHash returns the SHA-256 hex digest of the profile's canonical
// (sorted-key) JSON, so it cache-keys built images (BuiltProfileHash) the
// same regardless of Go struct field order.
//
// A marshal → unmarshal-into-map → marshal round trip gets canonical JSON via
// encoding/json's sorted map-key output, without hand-rolling a key sort.
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

// cacheKeySalt versions CacheKey's preimage. Bump it when a generator-bake
// change isn't already reflected in WorkspaceProfile's fields, so an old
// built image never false-cache-hits. v2: the agent-tool install became
// unconditional, so a v1 image may predate it and must rebuild.
const cacheKeySalt = "v2"

// CacheKey returns the SHA-256 digest keying a workspace's built-image cache:
// a salted digest of ProfileHash, not ProfileHash itself, so bumping
// cacheKeySalt forces a rebuild without colliding with it.
func (p WorkspaceProfile) CacheKey() string {
	sum := sha256.Sum256([]byte(p.ProfileHash() + "|" + cacheKeySalt))
	return hex.EncodeToString(sum[:])
}

// ToolchainNeeds reports which toolchain-fidelity accommodations a run over
// these profiles needs. Requirements-driven, not platform-wide: Go's
// tempdir/cache redirect applies only when Go was detected; the Maven/Gradle
// JVM proxy sysprops only when that package manager was detected.
func ToolchainNeeds(profiles ...WorkspaceProfile) (goNeeded, jvmNeeded bool) {
	for _, p := range profiles {
		if slices.Contains(p.Languages, "Go") {
			goNeeded = true
		}
		if slices.Contains(p.PackageManagers, "maven") || slices.Contains(p.PackageManagers, "gradle") {
			jvmNeeded = true
		}
	}
	return goNeeded, jvmNeeded
}
