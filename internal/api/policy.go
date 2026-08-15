// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	neturl "net/url"
	"os"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// confinementGE compares a run's class against a policy's MinConfinementClass.
// types.ConfinementClass.Rank is the single source of the ordering (it already
// ranks unrecognised values 0, so a gate on a real minimum fails closed); this
// stays a named helper because runs_create.go's "Membership, not rank (M8)"
// rationale points at it by name.
func confinementGE(have, want types.ConfinementClass) bool {
	return have.Rank() >= want.Rank()
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
	if spec.MinConfinementClass.Rank() == 0 {
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
	// Every domain entry must be a shape the proxy's matcher can actually
	// match. A dead entry (mid-label wildcard, URL, bad :port) reads as
	// protection but allows/denies nothing — reject it at every ingest point
	// (stored policy write, inline run policy, WARDYN_DEFAULT_POLICY file,
	// composer/profile clamp) rather than ship a policy that lies.
	for i, d := range spec.AllowedDomains {
		if err := proxy.ValidDomainEntry(d); err != nil {
			return fmt.Errorf("allowed_domains[%d]: %w", i, err)
		}
	}
	for i, d := range spec.DeniedDomains {
		if err := proxy.ValidDomainEntry(d); err != nil {
			return fmt.Errorf("denied_domains[%d]: %w", i, err)
		}
	}
	for i, g := range spec.EligibleGrants {
		if err := validateEligibleGrant(i, g); err != nil {
			return err
		}
	}
	if err := validateGrantLaneExclusivity(spec.EligibleGrants); err != nil {
		return err
	}
	if err := validatePolicyWorkspaces(spec); err != nil {
		return err
	}
	return nil
}

// validateEligibleGrant enforces the per-kind structural invariants of one
// eligible_grants entry (extracted from validatePolicySpec to keep each
// function under the gocyclo gate; behavior is identical).
func validateEligibleGrant(i int, g types.GrantSpec) error {
	switch g.Kind {
	case types.GrantGitHubToken, types.GrantCloudSTS, types.GrantAPIKey, types.GrantGitPAT, types.GrantSSHKey:
	default:
		return fmt.Errorf("eligible_grants[%d]: unknown kind %q", i, g.Kind)
	}
	if g.TTLSeconds < 0 {
		return fmt.Errorf("eligible_grants[%d]: negative ttl_seconds", i)
	}
	// A github_token grant's scope ({repos, permissions}) is otherwise only
	// checked at MINT time inside the broker, so a malformed permission key
	// surfaces as a run-time mint failure instead of an immediate 400. Run the
	// broker's OWN write-time predicate here — one implementation, so tightening
	// splitRepos can never leave policy-write accepting what mint now refuses.
	if g.Kind == types.GrantGitHubToken {
		if err := broker.ValidateGitHubScopeShape(g.Scope); err != nil {
			return fmt.Errorf("eligible_grants[%d]: github_token scope invalid: %w", i, err)
		}
	}
	// A cloud_sts grant is hard-denied by the embedded IdP (requires SPIRE) and
	// mints nothing, but a clearly-malformed scope should still be rejected at
	// write time rather than carried silently. Its scope is an (empty) JSON
	// object today; null/absent is fine, a non-object is not.
	if g.Kind == types.GrantCloudSTS && len(g.Scope) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(g.Scope, &obj); err != nil {
			return fmt.Errorf("eligible_grants[%d]: cloud_sts scope must be a JSON object: %w", i, err)
		}
	}
	// An api_key grant must never reference a reserved platform-internal secret
	// (wardyn-signing-key / wardyn-session-key): that would exfiltrate the
	// identity-signing or session-HMAC key as an injected Bearer header.
	// Reject at policy-write time (covers BOTH stored policies via POST
	// /policies and inline specs via resolveRunPolicy); the injection sink
	// enforces the same invariant defense-in-depth. A scope that does not
	// decode as an injection rule is left to the broker/sink — this check is
	// solely the reserved-name deny.
	//
	// The same decode also checks the authored HEADER NAME: it is written
	// verbatim onto a forwarded request by the proxy, so a name carrying CR/LF
	// is a header-splitting shape rather than a typo. The injection sink
	// (handleInternalInjection) enforces this too, defense-in-depth.
	if g.Kind == types.GrantAPIKey {
		if rule, derr := injectionRuleFromScope(g.Scope); derr == nil {
			if sinkReservedSecret(rule.SecretName) {
				return fmt.Errorf("eligible_grants[%d]: api_key references reserved secret name %q", i, rule.SecretName)
			}
			if !egress.ValidHeaderName(rule.Header) {
				return fmt.Errorf("eligible_grants[%d]: api_key header %q is not a valid HTTP header name", i, rule.Header)
			}
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
		if sinkReservedSecret(secretName) {
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
		if sinkReservedSecret(keyRef) || sinkReservedSecret(khRef) {
			return fmt.Errorf("eligible_grants[%d]: ssh_key references a reserved secret name", i)
		}
		if _, ok := sshOver443Endpoint(host); !ok {
			return fmt.Errorf("eligible_grants[%d]: ssh_key host %q is not a supported SSH-over-443 provider (github.com / dev.azure.com)", i, host)
		}
	}
	return nil
}

// validateGrantLaneExclusivity refuses a policy that declares BOTH a
// github_token grant and a second credential lane to the same forge — an
// ssh_key, or a git_pat. Brokered means
// SINGLE-LANE: on a brokered run the git-broker route is the only route to the
// forge by NAME, so every push is parsed and held inside refs/heads/wardyn/<run-id>/
// (confineGitBrokerEgress denies the managed hosts AND the forge's SSH endpoint;
// dropBrokeredGrants withholds the key itself). "By name" is the standing
// caveat those denies have always carried — see confineGitBrokerEgress; it is not
// new here. A co-granted ssh_key would hand the same run a second push path that
// SSH makes unparseable — so the operator picks one lane here, at write time,
// instead of being silently deprived of the endpoint at dispatch.
//
// DELIBERATELY STRICTER THAN DISPATCH. "Brokered" is decided at RUN time — the
// broker map is seeded from the github_token grant's scope.repos AND from the
// run's declared clone set (augmentGitBrokerGrants), so a grant with an empty repo
// scope is not brokered until a run supplies --repo. Policy-write therefore cannot
// know whether a given run will be brokered; the rule it enforces is DECLARATIVE
// (you may not declare both lanes to one forge), and the message says so.
//
// git_pat FOR A BROKERED FORGE IS COVERED TOO, since 2026-08-03. It used to be
// exempt on the reasoning that such a grant was "already dead twice over" —
// wardyn-git-helper refuses on isGitHubHost before the PAT fallback whenever
// WARDYN_GIT_BROKER_REPOS is set, and github.com is one of the four broker
// denies. The first of those is not a barrier: the helper is how *git* asks for a
// credential, and nothing obliges an agent to go through git — a POST to the mint
// route returns the PAT, because the proxy's mint refusal (isBrokeredGitGrant)
// matches github_token grant ids only. That left one barrier, a name-keyed egress
// deny that this repo documents as not binding a raw-IP CONNECT under
// allow_all_egress. And a GitHub git_pat is typically a USER PAT — wider than the
// repo-scoped installation token beside it, and bound by no branch namespace.
// A git_pat for a NON-brokered host (dev.azure.com, gitlab.com, GHES) is the
// ordinary supported lane and is untouched.
//
// Host matching across the three grant shapes: a github_token grant's scope is
// "<org>/<repo>" repos, so it implies the forge by construction (gitBrokerForges,
// github.com-only in v1); an ssh_key grant carries an explicit host, folded
// through sshOver443Endpoint (brokeredForgeSSHHost) — the same normalization
// validateEligibleGrant above already applies, and the one that treats
// "github.com" and "ssh.github.com" as the same forge; a git_pat carries the host
// git will dial, matched against gitBrokerForges host-or-subdomain
// (brokeredForgeHost), the same set wardyn-git-helper's isGitHubHost refuses on.
func validateGrantLaneExclusivity(grants []types.GrantSpec) error {
	brokered := false
	for _, g := range grants {
		if g.Kind == types.GrantGitHubToken {
			brokered = true
			break
		}
	}
	if !brokered {
		return nil
	}
	// A malformed scope was already rejected by validateEligibleGrant above, and
	// an unsupported host simply fails to match; both `continue`.
	for i, g := range grants {
		switch g.Kind {
		case types.GrantSSHKey:
			host, _, _, _, derr := sshKeyScopeFields(g.Scope)
			if derr != nil || !brokeredForgeSSHHost(host) {
				continue
			}
			return fmt.Errorf("eligible_grants[%d]: this policy declares BOTH a github_token grant and an ssh_key grant for %q — "+
				"a brokered forge is single-lane: the git-broker route is its only route by name, so every push it carries is parsed and "+
				"confined to refs/heads/wardyn/<run-id>/, while an ssh_key gives the same run a second push path SSH makes unparseable. Choose one: "+
				"drop the ssh_key grant to keep the brokered, branch-confined lane, or drop the github_token grant to push with your own "+
				"key, unbrokered and unbound", i, host)
		case types.GrantGitPAT:
			host, _, _, derr := gitPATScopeFields(g.Scope)
			if derr != nil || !brokeredForgeHost(host) {
				continue
			}
			return fmt.Errorf("eligible_grants[%d]: this policy declares BOTH a github_token grant and a git_pat grant for %q — "+
				"a brokered forge is single-lane: the git-broker route is its only route by name, so every push it carries is parsed and "+
				"confined to refs/heads/wardyn/<run-id>/, while a git_pat puts a resident token in the sandbox whose pushes are an opaque "+
				"CONNECT tunnel no parser can read — and a GitHub PAT is usually a user PAT, wider than the repo-scoped installation token. "+
				"Choose one: drop the git_pat grant to keep the brokered, branch-confined lane, or drop the github_token grant to push with "+
				"your own PAT, unbrokered and unbound. A git_pat for a non-brokered host (dev.azure.com, gitlab.com, GHES) is unaffected", i, host)
		}
	}
	return nil
}

// validatePolicyWorkspaces validates workspace_mounts/workspace_repos and the
// LLM-inspection block (extracted from validatePolicySpec for the gocyclo
// gate; behavior is identical).
func validatePolicyWorkspaces(spec types.RunPolicySpec) error {
	// WorkspaceMounts are operator/policy-controlled host bind mounts. Validate
	// each against the SAME deny-list the docker driver enforces (absolute,
	// cleaned, non-dangerous Source; allowed-prefix Target) so a bad mount is
	// rejected here at policy-write time (HTTP 400), not just defense-in-depth at
	// sandbox-create time. This is the policy half of the two-layer guardrail.
	//
	// WorkspaceRepos parallel WorkspaceMounts (multi-workspace run model): same
	// in-container-target shape check (runner.ValidateTarget, the extracted
	// target-prefix half of ValidateMount), and the two lists share ONE
	// unique-target invariant below so a clone can never land AT a bind
	// target's exact path (or shadow another repo's checkout). Exact equality
	// only: a repo target nested inside a mount target is allowed, and derived
	// default dests are never seen here (buildRepoRecords computes those).
	// Repo.Repo itself is not validated as
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
	if err := validateLLMInspection(spec); err != nil {
		return err
	}
	return nil
}

// validateLLMInspection enforces the structural invariants of the optional
// outbound content-inspection block. A nil spec (the default) is valid (off).
// Takes the FULL RunPolicySpec (not just LLMInspection) because
// detector_sidecar_url is now validated against this SAME spec's own egress
// allowlist — see the W12-A-1 comment below.
func validateLLMInspection(spec types.RunPolicySpec) error {
	li := spec.LLMInspection
	if li == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(li.Mode))
	switch mode {
	case "", "off", "alert", "block":
	default:
		return fmt.Errorf("llm_inspection.mode: unknown mode %q", li.Mode)
	}
	// W12-A-1/W12-S1-1: a raw VALUE may never be authored on a policy write —
	// stored, inline, or WARDYN_DEFAULT_POLICY. Only dispatch ever populates
	// this field, internally, in memory, on the ephemeral copy handed to the
	// proxy sidecar (resolveLLMInspectionSecrets, runs_dispatch.go). An
	// operator authors workspace_secret_names instead (types.LLMInspectionSpec).
	if len(li.WorkspaceSecretValues) > 0 {
		return fmt.Errorf("llm_inspection.workspace_secret_values may not be set on a policy write " +
			"(it is resolved internally, store->proxy, at dispatch — see workspace_secret_names)")
	}
	if mode != "" && mode != "off" {
		if !li.DetectSecrets && !li.DetectSecretPatterns && !li.DetectEntropy &&
			!li.DetectPII && li.DetectorSidecarURL == "" && len(li.ClassifiedMarkers) == 0 {
			return fmt.Errorf("llm_inspection: at least one detector must be enabled when mode is %q", mode)
		}
		if u := strings.TrimSpace(li.DetectorSidecarURL); u != "" {
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				return fmt.Errorf("llm_inspection.detector_sidecar_url must be an http(s) URL")
			}
			// W12-A-1 belt-and-braces: the sidecar is dialed PROXY-SIDE with the
			// outbound span TEXT (internal/contentscan/sidecar.go) — a surface the
			// sandbox's own confinement class never bounds, so "trusted operator
			// config" (that file's own framing) only actually holds once this
			// URL's host is ALSO on this same policy's own egress allowlist — the
			// same exact-entry bar a brokered credential injection already
			// requires (domainAllowedExact) — rather than accepting any http(s)
			// URL at face value. Doesn't widen egress by itself (the sidecar dial
			// never goes through the sandbox's own allowlist); it requires the
			// operator to have already named the host for something.
			parsed, perr := neturl.Parse(u)
			if perr != nil || parsed.Hostname() == "" {
				return fmt.Errorf("llm_inspection.detector_sidecar_url is not a valid URL")
			}
			if !spec.AllowAllEgress && !domainAllowedExact(spec.AllowedDomains, parsed.Hostname()) {
				return fmt.Errorf("llm_inspection.detector_sidecar_url host %q must be in this policy's own allowed_domains "+
					"(or allow_all_egress) — an operator allowlist, not a bare http(s) check", parsed.Hostname())
			}
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
