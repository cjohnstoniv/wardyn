// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ResourceID is Azure DevOps' first-party application id, the audience every
// Entra scope for Azure DevOps is qualified by. Fixed and tenant-invariant.
const ResourceID = "499b84ac-1321-427f-aa17-267ca6975798"

// neverRequestedScopes are the token-lifecycle scopes, unqualified. SECURITY:
// nothing in this package may ever put them in a capability's scope set —
// consent, not the request, decides an Entra token's scopes, and a token that
// carries one can create personal access tokens. Only the sign-in that mints a
// run's token asks for two of them (MintScopes), and never a capability.
var neverRequestedScopes = []string{
	"vso.tokens", "vso.pats", "vso.pats_manage", "vso.tokenadministration", "user_impersonation",
}

// MintScopes are the two delegated Azure DevOps permissions that let Wardyn
// create and revoke a person's personal access tokens, resource-qualified as
// an Entra request names them. They are not a capability's scopes and
// ScopesFor never returns them.
func MintScopes() []string {
	return []string{ResourceID + "/vso.pats", ResourceID + "/vso.pats_manage"}
}

// IsTokenScope reports whether s, qualified by the Azure DevOps resource (its id or its
// App ID URI) or not, is a scope that lets its holder create personal access tokens. A bearer
// token whose granted scopes name one must never ride a sandbox's traffic.
func IsTokenScope(s string) bool {
	s = strings.ToLower(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, ResourceID+"/"), resourceURI)
	return slices.Contains(neverRequestedScopes, s)
}

// resourceURI is the App ID URI form of the same resource, which a scope can
// also be qualified by. Lower case: IsTokenScope compares lower-cased.
const resourceURI = "https://app.vssps.visualstudio.com/"

// capabilityScopes is the capability -> Entra scope table, and THE ONE
// PLACE a capability becomes a scope: consent, the person's token request and
// any narrower credential all go through ScopesFor. SEVERAL CAPABILITIES SHARE
// ONE SCOPE (CapCodeWrite/CapPR/CapPolicyAdmin/CapPolicyBypass all resolve to
// vso.code_write, since ADO has no finer scope — the classifier, not the
// token, is what distinguishes them). ONE CAPABILITY MAY NEED SEVERAL
// (CapSecurityAdmin covers three).
//
// The READ rows are DERIVED from readAreas, not hand-written, since a
// hand-written copy of a read's scopes is exactly what drifted before.
var capabilityScopes = func() map[Capability][]string {
	m := map[Capability][]string{
		CapCodeWrite:    {"vso.code_write"},
		CapPR:           {"vso.code_write"},
		CapPolicyAdmin:  {"vso.code_write"},
		CapPolicyBypass: {"vso.code_write"},
		CapRepoAdmin:    {"vso.code_manage"},
		// Deleting work items and managing work tracking is served from the
		// same scope as editing; only the classifier tells them apart.
		CapWorkWrite: {"vso.work_write"},
		CapWorkAdmin: {"vso.work_write"},
		CapWikiWrite: {"vso.wiki_write"},
		// vso.build is the READ scope; queueing needs vso.build_execute, and
		// editing a definition is a write Azure DevOps also serves from it —
		// there is no separate "manage build definitions" scope.
		CapBuildExecute:         {"vso.build_execute"},
		CapBuildAdmin:           {"vso.build_execute"},
		CapReleaseExecute:       {"vso.release_execute"},
		CapReleaseAdmin:         {"vso.release_manage"},
		CapServiceEndpointAdmin: {"vso.serviceendpoint_manage"},
		CapPackagingWrite:       {"vso.packaging_write"},
		CapPackagingManage:      {"vso.packaging_manage"},
		CapProjectAdmin:         {"vso.project_manage"},
		// vso.security_manage does not reach the graph or identity APIs, and a
		// permission change that has to create a group needs both.
		CapSecurityAdmin: {"vso.security_manage", "vso.graph_manage", "vso.identity_manage"},
	}
	for _, a := range readAreas {
		if a.scope != "" && a.cap.Grantable() && !slices.Contains(m[a.cap], a.scope) {
			m[a.cap] = append(m[a.cap], a.scope)
		}
	}
	for c := range m {
		slices.Sort(m[c])
	}
	return m
}()

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

// PATScope is the scope string of the personal access token a run holds: the
// scopes of caps, unqualified (a PAT names `vso.code`, not the Entra resource
// form), sorted, deduplicated and space-joined. It is built from ScopesFor, so
// the capability -> scope table stays the one place that decides them.
// SECURITY: a capability that is not grantable is an error, and so is an empty
// set — a token created with no scope must never stand in for a narrow one.
func PATScope(caps []Capability) (string, error) {
	qualified, err := ScopesFor(caps)
	if err != nil {
		return "", err
	}
	if len(qualified) == 0 {
		return "", errors.New("adoscope: no capabilities — a personal access token needs at least one scope")
	}
	out := make([]string, len(qualified))
	for i, q := range qualified {
		out[i] = strings.TrimPrefix(q, ResourceID+"/")
	}
	return strings.Join(out, " "), nil
}

// Permits is THE gate: may a run holding granted perform v? The only
// spelling of "allowed" this package offers. SECURITY: comparison is over
// CAPABILITIES, never scopes — a scope comparison would permit essentially
// everything, since a token carries every scope consented to.
//
// DISCOVERY is the one verdict no list names: any run holding at least one
// grantable capability may make it, since it returns route templates, not
// organisation data.
func Permits(granted []Capability, v Verdict) bool {
	if v.Capability == CapDiscovery {
		return slices.ContainsFunc(granted, Capability.Grantable)
	}
	return v.Capability.Grantable() && slices.Contains(granted, v.Capability)
}
