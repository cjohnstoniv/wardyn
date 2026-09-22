// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"fmt"
	"slices"
)

// ResourceID is Azure DevOps' first-party application id — the audience every
// Entra scope for Azure DevOps is qualified by. It is a fixed Microsoft
// constant, the same value in every tenant, which is why it is a constant here
// and not a field on a provider row: a row carrying it could only ever carry
// it wrong.
const ResourceID = "499b84ac-1321-427f-aa17-267ca6975798"

// ScopeTokens is the scope the PAT-lifecycle API requires, and it is NOT in
// any capability's scope set below — deliberately, and this is the record of
// why a minted_pat row is accepted at the write boundary anyway:
//
// A row with token_mode: minted_pat needs this scope to mint the PAT at all,
// yet CapDeniedTokens means no CLASSIFIED request may ever use the token area.
// The two are consistent only because the mint happens on the control plane's
// own behalf, before a run exists, and the minted PAT is what the run sees.
// A run whose token carried this scope could mint itself a second credential
// outside every capability it was granted, so the run's token never does.
const ScopeTokens = "vso.tokens"

// readScopes are the scopes CapRead needs.
//
// It is the union of every area's READ scope rather than one scope, because
// ScopesFor is given capabilities and no AREA: the classifier answers CapRead
// for a read of code, work items, builds, releases, wikis, feeds, projects,
// service connections, identities, test results and analytics alike, so a
// token minted for "read" has to be able to perform any of them. Narrowing
// this to the area actually touched is a per-request mint, not a scope table.
//
// An area this package can answer CapRead for and is MISSING here is not a
// safety problem — it is a read that 403s at the forge — but it is a
// first-use failure on a route the operator believes they granted, so the two
// lists are meant to be kept level with each other.
var readScopes = []string{
	"vso.code", "vso.work", "vso.build", "vso.release", "vso.wiki",
	"vso.packaging", "vso.project", "vso.serviceendpoint", "vso.graph",
	"vso.identity", "vso.test", "vso.analytics",
}

// capabilityScopes is the capability -> Entra scope table.
//
// Two shapes in it are worth knowing about.
//
// SEVERAL CAPABILITIES SHARE ONE SCOPE: CapCodeWrite, CapPR, CapPolicyAdmin
// and CapPolicyBypass all resolve to vso.code_write because that is the only
// scope Azure DevOps offers for any of them. The capability, not the scope, is
// what distinguishes them — which is exactly why the catalogue exists, and why
// the classifier is the enforcing layer rather than a second opinion: an Entra
// access token carries every scope the person consented to, so the token
// itself does not bound a run.
//
// ONE CAPABILITY MAY NEED SEVERAL: the build capabilities cover both the build
// and the classic-release areas, which have separate scopes; CapSecurityAdmin
// covers permissions, the graph and identities, which have three.
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

// ScopesFor is the resource-qualified, deduplicated, sorted scope set for caps.
//
// A capability that is NOT GRANTABLE is an ERROR, not an empty result, and
// that distinction is load-bearing. A consumer gating with "the required
// scopes are inside the granted ones" would pass every unclassified write and
// every denied area on an empty set, because the empty set is inside
// everything — the fail-closed answer would have read as permission. Callers
// that want a yes/no should use Permits and never compare scope sets at all.
//
// Never returns nil for a non-empty input; returns an empty slice for an empty
// one, so a caller rendering JSON gets [] rather than null.
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

// Permits is THE gate: may a run holding granted perform v?
//
// It is the only spelling of "allowed" this package offers, and it is exported
// so that no consumer writes its own. The two ways to get this wrong are both
// closed here: a non-grantable capability (an unclassified write, a denied
// area) is never permitted whatever is granted, and the comparison is over
// CAPABILITIES rather than scopes — an Entra token carries every scope the
// person consented to, so a scope comparison would permit essentially
// everything.
func Permits(granted []Capability, v Verdict) bool {
	return v.Capability.Grantable() && slices.Contains(granted, v.Capability)
}
