// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// integrations_run.go is the RUNTIME half of the Integrations entity
// (integrations.go holds the entity, its validation and its capability
// matrix): what actually happens to a run's resolved policy when it is granted
// one.
//
// Two things happen, and nothing else:
//
//   - the integration's hosts join the run's egress allowlist — this is the
//     whole reason a host is reachable, instead of being hand-listed in every
//     workspace that needs it;
//   - a header-delivering integration authors ONE api_key grant per host,
//     through the SAME generic proxy-side injection mechanism every other
//     api_key grant already rides. Wardyn holds the secret, the proxy adds the
//     header, the sandbox never holds it.
//
// NOTHING IS AMBIENT. A run gets an integration when the workspace it runs in
// requires it (a `integration:<id>` requirement key, folded by
// applyWorkspaceRequirements) — never because the integration merely exists.
// An operator with fifty integrations configured and a workspace that names
// none of them gets a run whose spec is byte-identical to having none at all.

// applyIntegrationRequirement folds ONE granted integration into the run's
// resolved spec, returning the audit entry for the caller to record (launch
// does; preflight discards — see requirementAuditEntry).
//
// rows is the caller's already-computed effective integration set
// (s.effectiveIntegrations) — a caller folding several requirements in one
// request (applyWorkspaceRequirements, launchRecordRun) computes it ONCE and
// passes it to every call, instead of resolveIntegrationRef silently
// recomputing it (a full secret listing + subscription/Bedrock peek) once per
// requirement (PLATFORM-API-8).
//
// ok=false — no grant, no mutation, nothing audited — when the row cannot
// deliver anything: the id names nothing in rows, the row is Disabled, or it
// names no hosts. Those are silent degrades, matching applyRequiredSecretGrant's
// own rule that a missing/unusable credential must never brick a run — the
// Integrations surface is where the gap is visible.
//
// A per-capability off switch (DisabledCapabilities, PLATFORM-API-1) narrows
// which half applies: "egress_host" skips the host union, "credential" skips
// the injection — matching what the read matrix (applyDisabled,
// integrations.go) already reports for those cells, so an operator who turned
// a capability off there sees the SAME thing actually happen to a run, not a
// credential still silently presented on the wire.
//
// Hosts are unioned UNCONDITIONALLY (net of the egress_host switch above),
// even under AllowAllEgress: the proxy's credential injector requires an
// explicit exact allowlist entry and deliberately does not honor allow-all
// (Policy.AllowedExactHost), so without the entry an allow-all run would reach
// the host and still fail to present the credential.
func (s *Server) applyIntegrationRequirement(ctx context.Context, rows []integrationRow, spec *types.RunPolicySpec, id string) (requirementAuditEntry, bool) {
	integ, found := resolveIntegrationRefFrom(rows, id)
	if !found || integ.Disabled || len(integ.Egress) == 0 {
		return requirementAuditEntry{}, false
	}
	disabledCap := make(map[string]bool, len(integ.DisabledCapabilities))
	for _, capID := range integ.DisabledCapabilities {
		disabledCap[capID] = true
	}
	var addedEgress, grantedHosts []string
	if !disabledCap["egress_host"] {
		addedEgress = unionAllowedDomains(spec, integ.Egress)
	}
	if !disabledCap["credential"] {
		grantedHosts = s.applyIntegrationInjection(ctx, spec, integ)
	}
	if len(addedEgress) == 0 && len(grantedHosts) == 0 {
		// Everything this row offers was already on the spec (another workspace
		// requires the same integration, or a policy already listed its hosts),
		// or both capabilities are switched off. Nothing changed, so there is
		// nothing to audit.
		return requirementAuditEntry{}, false
	}
	_, header, _, _ := integ.HeaderSecret()
	return requirementAuditEntry{
		action: "run.workspace.requirement.integration", target: id,
		data: map[string]any{
			"integration_id": id, "added_domains": addedEgress,
			"injected_hosts": grantedHosts, "header": header,
		},
	}, true
}

// applyIntegrationInjection authors the proxy-side credential grants for a
// header-delivering integration — one api_key grant per host — and returns the
// hosts it granted. Empty (and a no-op) for a row that delivers no header, which
// is the honest state for every system that authenticates outside HTTP.
//
// ROLE-AGNOSTIC, DELIBERATELY (base-component model): the row's
// proxy_header-delivered secret is its credential whatever ROLE it carries —
// "token", "pat", "api_key" alike. Delivery is the contract; the role is a
// label for humans. So an AI-provider row that also names egress injects its
// key there too, which is the model's own promise: a connection's secrets and
// egress apply wherever the row is NAMED. Naming is the whole consent —
// nothing is ambient, a configured integration grants nothing until a
// workspace requires it (or a redirect points at it, resolveRedirectToken).
// A secret with NO declared delivery is still never matched: that is a closed
// kind's bespoke transport (the github_app halves, git_host's clone
// credentials), which this generic lane must not second-guess. At most ONE
// proxy_header secret can exist per row (validateIntegrationWrite), so
// HeaderSecret's "first match" IS every match.
//
// HONEST CEILING (pre-existing, not introduced here): the proxy injects a
// header on a plain-HTTP forward, or inside a TLS-MITM'd tunnel — and dispatch
// populates ProxyConfig.MITMHosts from the artifact-redirect plan and the
// Bedrock bearer host ONLY (runs_dispatch.go), never from an integration's
// egress. So on an https:// host this grant is authored, persisted and
// resolvable, but the CONNECT stays opaque and nothing is added to the wire.
// Widening MITM is a deliberate trust decision (see isCorpMITMHost's TRUST
// BOUNDARY note), not a side effect of folding an integration, so it is left
// to whoever makes it: the fix is to add these hosts to that same tightly
// scoped, injection-paired set.
//
// Three guards, each of which drops a host rather than failing the run:
//
//   - the named secret must actually be stored. An injection grant with no
//     resolvable secret fails the proxy CLOSED at startup, so an unstored one
//     would brick every run granted this integration; degrade to
//     path-open-no-credential instead, exactly as applyRequiredSecretGrant does.
//   - the host must be a BARE EXACT host. buildInjector refuses a rule whose
//     host misses the exact allowlist (a wildcard or ":port" entry compiles
//     elsewhere), and that refusal is a hard startup failure. The write path
//     already rejects this combination (validateIntegrationHosts); this covers a
//     row stored before that guard, or a legacy-derived one.
//   - a host that already has an api_key grant is left alone — never
//     double-grant a host, mirroring ensureLLMGrant/applyWorkspaceCreds.
//     Whichever caller proposed it first wins.
func (s *Server) applyIntegrationInjection(ctx context.Context, spec *types.RunPolicySpec, integ types.Integration) []string {
	// HeaderSecret is the row's proxy_header-delivered secret; an empty stored
	// format is already materialized as "%s" (the raw secret IS the header
	// value — the right default for a custom credential header like x-api-key
	// or DD-API-KEY; injectionRuleFromScope defaults an empty format to
	// "Bearer %s", which would be wrong for every one of those).
	secretName, header, format, ok := integ.HeaderSecret()
	if !ok || secretName == "" || !s.secretPresent(ctx, secretName) {
		return nil
	}
	var granted []string
	for _, host := range integ.Egress {
		if !bareExactHost(host) {
			continue
		}
		if _, exists := apiKeyGrantForHost(spec, host); exists {
			continue
		}
		scope, _ := json.Marshal(map[string]string{
			"host": host, "header": header, "format": format, "secret_name": secretName,
		})
		spec.EligibleGrants = append(spec.EligibleGrants, types.GrantSpec{
			Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600, RequiresApproval: false,
		})
		granted = append(granted, host)
	}
	return granted
}
