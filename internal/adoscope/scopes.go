// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"fmt"
	"slices"
)

// ResourceID is Azure DevOps' first-party application id — the audience
// every Entra scope for Azure DevOps is qualified by. A fixed Microsoft
// constant, the same in every tenant, so it is a const here rather than a
// field on a provider row.
const ResourceID = "499b84ac-1321-427f-aa17-267ca6975798"

// neverRequestedScopes are the token-lifecycle scopes, and nothing in this
// package may ever put them in a scope or consent set: Azure DevOps mints
// PATs only for Microsoft's own first-party clients (measured: 401
// TF400813), and consent rather than the request decides a token's scopes,
// so once consented these would ride along in every run's token.
var neverRequestedScopes = []string{"vso.tokens", "vso.pats"}

// readScopes are the scopes CapRead needs: every non-empty scope in
// readAreas, sorted. DERIVED rather than written out because it is the
// union of every area's read scope, and a hand-written copy of that union is
// exactly what drifted before.
var readScopes = func() []string {
	var out []string
	for _, s := range readAreas {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}()

// capabilityScopes is the capability -> Entra scope table. Two shapes worth
// knowing: SEVERAL CAPABILITIES SHARE ONE SCOPE (CapCodeWrite, CapPR,
// CapPolicyAdmin, CapPolicyBypass all resolve to vso.code_write, since that
// is the only scope ADO offers for any of them — the capability, not the
// scope, is what distinguishes them, which is why the classifier enforces
// rather than the token). ONE CAPABILITY MAY NEED SEVERAL (build covers both
// build and classic-release areas; CapSecurityAdmin covers three).
var capabilityScopes = map[Capability][]string{
	CapRead:         readScopes,
	CapCodeWrite:    {"vso.code_write"},
	CapPR:           {"vso.code_write"},
	CapPolicyAdmin:  {"vso.code_write"},
	CapPolicyBypass: {"vso.code_write"},
	CapRepoAdmin:    {"vso.code_manage"},
	// vso.security_manage does not reach the graph or identity APIs, and a
	// permission change that has to create a group needs both.
	CapSecurityAdmin:        {"vso.security_manage", "vso.graph_manage", "vso.identity_manage"},
	CapServiceEndpointAdmin: {"vso.serviceendpoint_manage"},
	// vso.build is the READ scope; queueing needs vso.build_execute, and the
	// classic-release equivalent is vso.release_execute.
	CapBuildExecute: {"vso.build_execute", "vso.release_execute"},
	// Editing a definition is a WRITE on the build area, which Azure DevOps
	// also serves from vso.build_execute — there is no separate "manage build
	// definitions" scope — and vso.release_manage on the release side.
	CapBuildAdmin:     {"vso.build_execute", "vso.release_manage"},
	CapWorkWrite:      {"vso.work_write"},
	CapWikiWrite:      {"vso.wiki_write"},
	CapPackagingWrite: {"vso.packaging_write"},
	CapProjectAdmin:   {"vso.project_manage"},
}

// ScopesFor is the resource-qualified, deduplicated, sorted scope set for
// caps. A capability that is NOT GRANTABLE is an ERROR, not an empty result:
// a consumer gating with "the required scopes are inside the granted ones"
// would pass every unclassified write on an empty set, since the empty set
// is inside everything. Callers that want a yes/no should use Permits.
// Never returns nil for a non-empty input.
func ScopesFor(caps []Capability) ([]string, error) {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if !c.Grantable() {
			return nil, fmt.Errorf("adoscope: %q is not a grantable capability — it has no scopes and must be refused, not minted", c)
		}
		for _, s := range capabilityScopes[c] {
			q := ResourceID + "/" + s
			if !slices.Contains(out, q) {
				out = append(out, q)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// Permits is THE gate: may a run holding granted perform v? The only
// spelling of "allowed" this package offers. A non-grantable capability is
// never permitted whatever is granted, and the comparison is over
// CAPABILITIES rather than scopes — a scope comparison would permit
// essentially everything, since a token carries every scope consented to.
func Permits(granted []Capability, v Verdict) bool {
	return v.Capability.Grantable() && slices.Contains(granted, v.Capability)
}
