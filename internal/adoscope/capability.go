// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package adoscope is the Azure DevOps CAPABILITY CATALOGUE: it names the
// access an Azure DevOps request needs, classifies one request to exactly one
// name, and maps a set of those names to the Entra scopes a token must carry.
//
// It exists because "authorize this person as themselves" is unanswerable
// without a vocabulary: Azure DevOps asks for vso.code_write both to push a
// commit and to complete a pull request while BYPASSING branch policy, so a
// token minted from the scope the API asks for is strictly wider than the work
// it was minted for. The capability names below are what a run is held to; the
// scope strings are only what Entra understands.
//
// THE PACKAGE IS PURE and imports the STANDARD LIBRARY ONLY. Two constraints
// keep it that way:
//   - internal/types names Capability on a git-provider row, so importing
//     internal/types here would be an import cycle.
//   - the classifier is the one opinion about what a request needs. Importing
//     internal/api or internal/egress would let a call site's own idea of a
//     "read" leak in beside it, which is how two spellings of one rule start.
//
// FAIL-CLOSED IS THE CONTRACT. Classify returns an error for a request it
// cannot reason about and CapUnclassifiedWrite for a write it does not
// recognize; neither is grantable, so a caller refuses both. A classifier that
// guessed would hand out a scope on a route nobody reviewed.
package adoscope

import (
	"maps"
	"slices"
)

// Capability is the ONE access name a classified request needs. The set is
// closed: a request is one of these or it is refused.
type Capability string

// The GRANTABLE capabilities — the only values a provider row's ceiling or
// profile may name, and the only ones ScopesFor turns into scopes.
const (
	// CapRead is a read of any area in readAreas: the floor every profile
	// starts from, and a ceiling must include it.
	//
	// It is WIDER than "read the code". The read scope set covers every area
	// the catalogue answers CapRead for, so the minimum Azure DevOps credential
	// can also read service-connection configuration, the organisation's
	// groups and users, and directory identities. That is inherent to minting
	// by capability rather than by area, and the label says so.
	CapRead Capability = "read"
	// CapCodeWrite is a commit, a push or a ref move that no branch policy
	// protects.
	CapCodeWrite Capability = "code_write"
	// CapPR is creating, updating, reviewing or completing a pull request
	// WITHOUT bypassing policy.
	CapPR Capability = "pr"
	// CapPolicyAdmin is editing the branch policies themselves.
	CapPolicyAdmin Capability = "policy_admin"
	// CapPolicyBypass is landing a change THROUGH a policy rather than past
	// it: a PR completed with bypassPolicy, or a ref moved on a protected
	// branch. Azure DevOps asks only for write scope for both, which is the
	// whole reason this capability is spelled separately from CapCodeWrite.
	CapPolicyBypass Capability = "policy_bypass"
	// CapRepoAdmin is creating, renaming or deleting a repository.
	CapRepoAdmin Capability = "repo_admin"
	// CapSecurityAdmin is changing permissions, ACLs or directory identities.
	CapSecurityAdmin Capability = "security_admin"
	// CapServiceEndpointAdmin is changing a service connection — the object
	// that holds someone else's cloud credential.
	CapServiceEndpointAdmin Capability = "serviceendpoint_admin"
	// CapBuildExecute is queueing a pipeline run — and, because the classic
	// release area folds onto the build capabilities in this catalogue,
	// creating, deploying and deleting releases. The label says both.
	CapBuildExecute Capability = "build_execute"
	// CapBuildAdmin is editing a pipeline or release definition — deciding
	// what every future run and release executes.
	CapBuildAdmin Capability = "build_admin"
	// CapWorkWrite is writing work items, including through the batch door.
	CapWorkWrite Capability = "work_write"
	// CapWikiWrite is writing a wiki.
	CapWikiWrite Capability = "wiki_write"
	// CapPackagingWrite is publishing to a feed.
	CapPackagingWrite Capability = "packaging_write"
	// CapProjectAdmin is creating, changing or deleting a project.
	CapProjectAdmin Capability = "project_admin"
)

// The REFUSED capabilities. They are Capability values rather than errors so a
// refusal can name the area it refused — an operator reading "denied_tokens"
// learns something "refused" does not tell them — but none of them is
// grantable, so a caller refuses every one.
const (
	// CapDeniedTokens is the personal-access-token and token-administration
	// area. A token lane that can mint or revoke tokens is a lane that can
	// leave the lane.
	CapDeniedTokens Capability = "denied_tokens"
	// CapDeniedServiceHooks is the service-hook area: a subscription that
	// forwards the org's events to an address of the caller's choosing is
	// exfiltration with a webhook's paperwork.
	CapDeniedServiceHooks Capability = "denied_service_hooks"
	// CapDeniedExtensions is extension management: installing an extension
	// runs someone else's code inside the organisation.
	CapDeniedExtensions Capability = "denied_extension_management"
	// CapDeniedInternal is the undocumented contribution API the web UI drives
	// itself with (_apis/Contribution/…). It is a single door onto every other
	// area, so no capability over it could mean anything.
	CapDeniedInternal Capability = "denied_internal"
	// CapUnclassifiedWrite is a write this catalogue does not recognize. It is
	// the fail-closed answer, not a capability anyone can hold.
	CapUnclassifiedWrite Capability = "unclassified_write"
	// CapUnclassifiedRead is a read of an area outside readAreas — one whose
	// read no scope in the read set could perform. Refused like its write
	// twin, rather than classified as a read that 403s at the forge.
	CapUnclassifiedRead Capability = "unclassified_read"
)

// grantableCapabilities is the closed grantable set — the only values a
// ceiling, a profile or a scope request may name.
var grantableCapabilities = map[Capability]bool{
	CapRead: true, CapCodeWrite: true, CapPR: true, CapPolicyAdmin: true,
	CapPolicyBypass: true, CapRepoAdmin: true, CapSecurityAdmin: true,
	CapServiceEndpointAdmin: true, CapBuildExecute: true, CapBuildAdmin: true,
	CapWorkWrite: true, CapWikiWrite: true, CapPackagingWrite: true,
	CapProjectAdmin: true,
}

// deniedCapabilities is the closed refused set.
var deniedCapabilities = map[Capability]bool{
	CapDeniedTokens: true, CapDeniedServiceHooks: true,
	CapDeniedExtensions: true, CapDeniedInternal: true,
}

// Grantable reports whether c may be written on a provider row and turned into
// a scope. Everything else — the denied areas and the unclassified write — is
// refused, which is why a caller tests THIS rather than enumerating refusals.
func (c Capability) Grantable() bool { return grantableCapabilities[c] }

// Denied reports whether c names an area no capability can be held over.
func (c Capability) Denied() bool { return deniedCapabilities[c] }

// Valid reports whether c is a value Classify can return at all.
func (c Capability) Valid() bool {
	return c.Grantable() || c.Denied() || c == CapUnclassifiedWrite || c == CapUnclassifiedRead
}

// GrantableCapabilities is the grantable set in a stable order, for the
// "want one of: …" half of a rejected write and for a UI's picker.
func GrantableCapabilities() []Capability {
	return slices.Sorted(maps.Keys(grantableCapabilities))
}

// GrantableCapabilityList is GrantableCapabilities as strings, the shape an
// error message joins.
func GrantableCapabilityList() []string {
	caps := GrantableCapabilities()
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = string(c)
	}
	return out
}

// labels are the plain-language rendering of every capability, in the SECOND
// person, because the sentence a person reads is "this run may …".
//
// They live in this package on purpose: a console that wrote its own wording
// would be a second, drifting definition of what a capability permits, and the
// label is the only part of a capability most people will ever read.
var labels = map[Capability]string{
	CapRead:                 "Read code, work items, pipelines, releases, wikis and feeds — including service connection settings, the organisation's groups and users, and directory identities",
	CapCodeWrite:            "Push commits and move branches that no policy protects",
	CapPR:                   "Open, review and complete pull requests",
	CapPolicyAdmin:          "Change the branch policies themselves",
	CapPolicyBypass:         "Land changes past a branch policy",
	CapRepoAdmin:            "Create, rename and delete repositories",
	CapSecurityAdmin:        "Change permissions and identities",
	CapServiceEndpointAdmin: "Change service connections and the credentials they hold",
	CapBuildExecute:         "Queue pipeline runs, and create, deploy and delete releases",
	CapBuildAdmin:           "Change pipeline and release definitions — what every future run and release executes",
	CapWorkWrite:            "Create and update work items",
	CapWikiWrite:            "Write wiki pages",
	CapPackagingWrite:       "Publish packages to feeds",
	CapProjectAdmin:         "Create, change and delete projects",

	CapDeniedTokens:       "Not available: minting or revoking access tokens",
	CapDeniedServiceHooks: "Not available: forwarding organisation events to another address",
	CapDeniedExtensions:   "Not available: installing extensions into the organisation",
	CapDeniedInternal:     "Not available: the organisation's internal web API",
	CapUnclassifiedWrite:  "Not available: a write this deployment does not recognize",
	CapUnclassifiedRead:   "Not available: a read this deployment does not recognize",
}

// Label is c's plain-language rendering, or "" for a value outside the set —
// an empty string rather than a guess, so a caller cannot render an
// unrecognized capability as though it were understood.
func Label(c Capability) string { return labels[c] }

// ProfileRead is the DEFAULT profile: reads and nothing else. A provider row
// whose default_profile is empty gets this one, which is what makes the
// read-only default a fact of the catalogue rather than a field someone
// remembered to fill in.
func ProfileRead() []Capability { return []Capability{CapRead} }

// ProfileContribute is the "do the work" profile: read, push, pull requests,
// work items and wiki.
//
// CapBuildExecute is deliberately ABSENT. Queueing a pipeline executes YAML
// under the pipeline's own identity, which is a different and usually wider
// identity than the person contributing — so it is an opt-in on the row, never
// part of what "contribute" means.
func ProfileContribute() []Capability {
	return []Capability{CapRead, CapCodeWrite, CapPR, CapWorkWrite, CapWikiWrite}
}
