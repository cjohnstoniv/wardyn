// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
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
// for a read of code, work items, builds, wikis, feeds and projects alike, so
// a token minted for "read" has to be able to perform any of them. Narrowing
// this to the area actually touched is a per-request mint, not a scope table.
var readScopes = []string{
	"vso.code", "vso.work", "vso.build", "vso.wiki", "vso.packaging", "vso.project",
}

// capabilityScopes is the capability -> Entra scope table.
//
// The interesting rows are the ones where SEVERAL capabilities share a scope:
// CapCodeWrite, CapPR, CapPolicyAdmin and CapPolicyBypass all resolve to
// vso.code_write because that is the only scope Azure DevOps offers for any of
// them. The capability, not the scope, is what distinguishes them — which is
// exactly why the catalogue exists.
var capabilityScopes = map[Capability][]string{
	CapRead:                 readScopes,
	CapCodeWrite:            {"vso.code_write"},
	CapPR:                   {"vso.code_write"},
	CapPolicyAdmin:          {"vso.code_write"},
	CapPolicyBypass:         {"vso.code_write"},
	CapRepoAdmin:            {"vso.code_manage"},
	CapSecurityAdmin:        {"vso.security_manage"},
	CapServiceEndpointAdmin: {"vso.serviceendpoint_manage"},
	CapBuildExecute:         {"vso.build_execute"},
	CapBuildAdmin:           {"vso.build"},
	CapWorkWrite:            {"vso.work_write"},
	CapWikiWrite:            {"vso.wiki_write"},
	CapPackagingWrite:       {"vso.packaging_write"},
	CapProjectAdmin:         {"vso.project_manage"},
}

// ScopesFor is the resource-qualified, deduplicated, sorted scope set for caps.
//
// A capability that is not grantable contributes NOTHING and is not an error:
// ScopesFor is the mint's input, and the refusal of a denied or unclassified
// capability belongs at the classification site that produced it, not here.
// Silently minting a scope for it is the only outcome this must never have.
//
// Never returns nil for a non-empty grantable input; returns an empty slice
// for an empty or wholly non-grantable one, so a caller rendering JSON gets
// [] rather than null.
func ScopesFor(caps []Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		for _, s := range capabilityScopes[c] {
			q := ResourceID + "/" + s
			if !slices.Contains(out, q) {
				out = append(out, q)
			}
		}
	}
	slices.Sort(out)
	return out
}
