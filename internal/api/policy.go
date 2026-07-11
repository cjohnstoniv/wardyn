// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// confinementRank orders Confinement Classes so policy gating can compare a
// policy's MinConfinementClass against what a runner declares. Unrecognised
// values rank 0 (below CC1) so they never satisfy a real minimum (fail closed).
var confinementRank = map[types.ConfinementClass]int{
	types.CC1: types.CC1.Rank(),
	types.CC2: types.CC2.Rank(),
	types.CC3: types.CC3.Rank(),
}

func confinementGE(have, want types.ConfinementClass) bool {
	return confinementRank[have] >= confinementRank[want]
}

// LoadPolicySpec reads and validates a RunPolicySpec from a JSON file. Used by
// wardynd to seed the default policy from examples/policies/default.json.
func LoadPolicySpec(path string) (types.RunPolicySpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return types.RunPolicySpec{}, fmt.Errorf("api: read policy %s: %w", path, err)
	}
	var spec types.RunPolicySpec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return types.RunPolicySpec{}, fmt.Errorf("api: parse policy %s: %w", path, err)
	}
	if err := validatePolicySpec(spec); err != nil {
		return types.RunPolicySpec{}, fmt.Errorf("api: invalid policy %s: %w", path, err)
	}
	return spec, nil
}

// validatePolicySpec enforces the minimal structural invariants a policy must
// satisfy before any run can be scheduled against it.
func validatePolicySpec(spec types.RunPolicySpec) error {
	if spec.MinConfinementClass == "" {
		return fmt.Errorf("min_confinement_class is required")
	}
	if _, ok := confinementRank[spec.MinConfinementClass]; !ok {
		return fmt.Errorf("unknown min_confinement_class %q", spec.MinConfinementClass)
	}
	if !spec.FirstUseApproval.Valid() {
		return fmt.Errorf("invalid first_use_approval %q (want always_deny, deny_with_review, or wait_for_review)", spec.FirstUseApproval)
	}
	// EGRESS modes. Two are valid and BOTH accept an empty allowed_domains:
	//   - default-deny (allow_all_egress=false): an empty allowlist is a
	//     deny-all policy, so we must NOT newly require domains here.
	//   - allow-all / deny-list-only (allow_all_egress=true): the proxy allows
	//     any non-denied PUBLIC host; denied_domains still wins.
	// first_use_approval is INERT under allow-all (allow-all wins — every
	// non-denied host is already allowed, so nothing escalates to approval).
	// We leave the field as-authored (the proxy ignores it under allow-all);
	// no rejection is warranted. The SSRF/private-IP guard and the exact-entry
	// requirement for credential injection are enforced by the proxy and are
	// unaffected by this mode.
	for i, g := range spec.EligibleGrants {
		switch g.Kind {
		case types.GrantGitHubToken, types.GrantCloudSTS, types.GrantAPIKey, types.GrantGitPAT, types.GrantSSHKey:
		default:
			return fmt.Errorf("eligible_grants[%d]: unknown kind %q", i, g.Kind)
		}
		if g.TTLSeconds < 0 {
			return fmt.Errorf("eligible_grants[%d]: negative ttl_seconds", i)
		}
		// An api_key grant must never reference a reserved platform-internal secret
		// (wardyn-signing-key / wardyn-session-key): that would exfiltrate the
		// identity-signing or session-HMAC key as an injected Bearer header.
		// Reject at policy-write time (covers BOTH stored policies via POST
		// /policies and inline specs via resolveRunPolicy); the injection sink
		// enforces the same invariant defense-in-depth. A scope that does not
		// decode as an injection rule is left to the broker/sink — this check is
		// solely the reserved-name deny.
		if g.Kind == types.GrantAPIKey {
			if rule, derr := injectionRuleFromScope(g.Scope); derr == nil && reservedSecret(rule.SecretName) {
				return fmt.Errorf("eligible_grants[%d]: api_key references reserved secret name %q", i, rule.SecretName)
			}
		}
		// A git_pat grant returns the STORED PAT VALUE to the git credential
		// helper (unlike api_key, whose value never leaves the broker). Require
		// host + secret_name and reject a reserved platform-internal secret at
		// WRITE time — fail closed so a policy can never exfiltrate
		// wardyn-signing-key/session-key as a git password. The broker sink
		// (mintGitPAT) enforces the same invariant defense-in-depth.
		if g.Kind == types.GrantGitPAT {
			_, secretName, _, derr := gitPATScopeFields(g.Scope)
			if derr != nil {
				return fmt.Errorf("eligible_grants[%d]: git_pat scope invalid: %w", i, derr)
			}
			if reservedSecret(secretName) {
				return fmt.Errorf("eligible_grants[%d]: git_pat references reserved secret name %q", i, secretName)
			}
		}
		// An ssh_key grant materializes a RESIDENT private key (see GrantSSHKey).
		// Require host + key_secret_ref, reject a reserved platform-internal secret
		// as either the key or the known_hosts ref, and require the host to be one
		// of the SSH-over-443 providers Wardyn supports (github.com / dev.azure.com)
		// so the run never asks for a resident key for an unroutable host. Fail
		// closed at WRITE time; the broker sink (mintSSHKey) re-checks the secrets.
		if g.Kind == types.GrantSSHKey {
			host, keyRef, _, khRef, derr := sshKeyScopeFields(g.Scope)
			if derr != nil {
				return fmt.Errorf("eligible_grants[%d]: ssh_key scope invalid: %w", i, derr)
			}
			if reservedSecret(keyRef) || reservedSecret(khRef) {
				return fmt.Errorf("eligible_grants[%d]: ssh_key references a reserved secret name", i)
			}
			if _, ok := sshOver443Endpoint(host); !ok {
				return fmt.Errorf("eligible_grants[%d]: ssh_key host %q is not a supported SSH-over-443 provider (github.com / dev.azure.com)", i, host)
			}
		}
	}
	// WorkspaceMounts are operator/policy-controlled host bind mounts. Validate
	// each against the SAME deny-list the docker driver enforces (absolute,
	// cleaned, non-dangerous Source; allowed-prefix Target) so a bad mount is
	// rejected here at policy-write time (HTTP 400), not just defense-in-depth at
	// sandbox-create time. This is the policy half of the two-layer guardrail.
	//
	// WorkspaceRepos parallel WorkspaceMounts (multi-workspace run model): same
	// in-container-target shape check (runner.ValidateTarget, the extracted
	// target-prefix half of ValidateMount), and the two lists share ONE
	// unique-target invariant below so a clone can never land on a bind target
	// (or shadow another repo's checkout). Repo.Repo itself is not validated as
	// an onboarded source here — gating a run to only ONBOARDED workspaces is a
	// later, security-critical wave (validateWorkspaceSources); this stays the
	// PURE structural check (no store access).
	seenTargets := make(map[string]bool, len(spec.WorkspaceMounts)+len(spec.WorkspaceRepos))
	for i, wm := range spec.WorkspaceMounts {
		if err := runner.ValidateMount(runner.Mount{
			Source:   wm.Source,
			Target:   wm.Target,
			ReadOnly: wm.ReadOnlyOrDefault(),
		}); err != nil {
			return fmt.Errorf("workspace_mounts[%d]: %w", i, err)
		}
		if seenTargets[wm.Target] {
			return fmt.Errorf("workspace_mounts[%d]: target %q duplicates another workspace_mounts/workspace_repos entry", i, wm.Target)
		}
		seenTargets[wm.Target] = true
	}
	for i, wr := range spec.WorkspaceRepos {
		if wr.Target == "" {
			// No explicit dest: the default (~/work/<name> convention) is derived
			// by a LATER wave (WARDYN_REPOS); nothing to collide-check here yet.
			continue
		}
		if err := runner.ValidateTarget(wr.Target); err != nil {
			return fmt.Errorf("workspace_repos[%d]: %w", i, err)
		}
		if seenTargets[wr.Target] {
			return fmt.Errorf("workspace_repos[%d]: target %q duplicates another workspace_mounts/workspace_repos entry", i, wr.Target)
		}
		seenTargets[wr.Target] = true
	}
	if err := validateLLMInspection(spec.LLMInspection); err != nil {
		return err
	}
	return nil
}

// validateLLMInspection enforces the structural invariants of the optional
// outbound content-inspection block. A nil spec (the default) is valid (off).
// v1 implements only the known-secret detector; entropy/PII toggles are rejected
// until their later-phase detectors land, so a policy can never silently request
// a detector that does not run.
func validateLLMInspection(li *types.LLMInspectionSpec) error {
	if li == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(li.Mode))
	switch mode {
	case "", "off", "alert", "block":
	default:
		return fmt.Errorf("llm_inspection.mode: unknown mode %q", li.Mode)
	}
	if mode != "" && mode != "off" {
		if !li.DetectSecrets && !li.DetectSecretPatterns && !li.DetectEntropy &&
			!li.DetectPII && li.DetectorSidecarURL == "" && len(li.ClassifiedMarkers) == 0 {
			return fmt.Errorf("llm_inspection: at least one detector must be enabled when mode is %q", mode)
		}
		if u := strings.TrimSpace(li.DetectorSidecarURL); u != "" &&
			!strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("llm_inspection.detector_sidecar_url must be an http(s) URL")
		}
		// require_inspectable_llm is a RUNTIME guarantee, and only TLS-MITM can
		// inspect an opaque CONNECT (incl. an api-key run that overrides its base
		// URL to CONNECT directly). So requiring inspectability requires MITM.
		if li.RequireInspectableLLM && !li.InterceptTLS {
			return fmt.Errorf("llm_inspection: require_inspectable_llm requires intercept_tls (only TLS-MITM gives a runtime inspection guarantee)")
		}
	}
	if li.MaxScanBytes < 0 {
		return fmt.Errorf("llm_inspection.max_scan_bytes must be >= 0")
	}
	switch strings.ToLower(strings.TrimSpace(li.OnScannerError)) {
	case "", "pass", "block":
	default:
		return fmt.Errorf("llm_inspection.on_scanner_error: must be \"pass\" or \"block\"")
	}
	switch strings.ToLower(strings.TrimSpace(li.BlockMinSeverity)) {
	case "", "low", "medium", "high", "critical":
	default:
		return fmt.Errorf("llm_inspection.block_min_severity: unknown severity %q", li.BlockMinSeverity)
	}
	return nil
}
