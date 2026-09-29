// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"fmt"
	"slices"
)

// ResourceID is Azure DevOps' first-party application id, the audience every
// Entra scope for Azure DevOps is qualified by. Fixed and tenant-invariant.
const ResourceID = "499b84ac-1321-427f-aa17-267ca6975798"

// neverRequestedScopes are the token-lifecycle scopes. SECURITY: nothing in
// this package may ever put them in a scope or consent set — consent, not the
// request, decides a token's scopes, so once consented these would ride along
// in every run's token (Azure DevOps mints PATs only for Microsoft's own
// first-party clients; measured: 401 TF400813).
var neverRequestedScopes = []string{"vso.tokens", "vso.pats"}

// readScopes are the scopes CapRead needs: every non-empty scope in
// readAreas, sorted. DERIVED, not hand-written, since a hand-written copy of
// this union is exactly what drifted before.
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

// capabilityScopes is the capability -> Entra scope table. SEVERAL
// CAPABILITIES SHARE ONE SCOPE (e.g. CapCodeWrite/CapPR/CapPolicyAdmin/
// CapPolicyBypass all resolve to vso.code_write, since ADO has no finer
// scope — the classifier, not the token, is what distinguishes them). ONE
// CAPABILITY MAY NEED SEVERAL (build covers build + classic-release areas;
// CapSecurityAdmin covers three).
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
// caps. SECURITY: a capability that is NOT GRANTABLE is an ERROR, not an
// empty result — a consumer gating with "required scopes are inside the
// granted ones" would pass every unclassified write on an empty set, since
// the empty set is inside everything. Callers wanting yes/no should use
// Permits. Never returns nil for a non-empty input.
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
// spelling of "allowed" this package offers. SECURITY: comparison is over
// CAPABILITIES, never scopes — a scope comparison would permit essentially
// everything, since a token carries every scope consented to.
func Permits(granted []Capability, v Verdict) bool {
	return v.Capability.Grantable() && slices.Contains(granted, v.Capability)
}
