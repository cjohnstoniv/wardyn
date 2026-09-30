// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package adoscope is the Azure DevOps CAPABILITY CATALOGUE: it names the
// access a request needs, classifies one request to exactly one name, and
// maps those names to Entra scopes — because Azure DevOps asks for
// vso.code_write both to push a commit and to complete a pull request while
// BYPASSING branch policy, so a token minted from the requested scope is
// strictly wider than the work it was minted for.
//
// The package is PURE, standard-library only (importing internal/types would
// cycle; internal/api or internal/egress would let a call site's own idea of
// "read" drift from the classifier's).
//
// FAIL-CLOSED IS THE CONTRACT: Classify errors on a request it can't reason
// about, and returns CapUnclassifiedWrite for an unrecognized write; neither
// is grantable, so a caller refuses both.
package adoscope

import (
	"maps"
	"slices"
	"strings"
)

// Capability is the ONE access name a classified request needs. The set is
// closed: a request is one of these or it is refused.
type Capability string

// The GRANTABLE capabilities — the only values a provider row's ceiling or
// profile may name, and the only ones ScopesFor turns into scopes. One read
// per Azure DevOps area, then that area's write and admin rows, following
// Azure DevOps' own read → write → manage scope ladder.
const (
	// CapCodeRead is a read of the git and policy areas, a pull-request query,
	// code search, and a clone or fetch through the git broker.
	CapCodeRead Capability = "code_read"
	// CapCodeWrite is a commit, a push or a ref move. The caller holds a ref
	// move to the run's own branch namespace unless the run's policy sets
	// git_push_any_branch; that rule is the run's, never a capability's.
	CapCodeWrite Capability = "code_write"
	// CapPR is creating, updating, reviewing or completing a pull request
	// WITHOUT bypassing policy.
	CapPR Capability = "pr"
	// CapPolicyAdmin is editing the branch policies themselves.
	CapPolicyAdmin Capability = "policy_admin"
	// CapPolicyBypass is completing a pull request with
	// completionOptions.bypassPolicy — the one request whose body asks Azure
	// DevOps to skip its own policies. Azure DevOps asks only write scope for
	// it, hence the split from CapPR.
	CapPolicyBypass Capability = "policy_bypass"
	// CapRepoAdmin is creating, renaming or deleting a repository.
	CapRepoAdmin Capability = "repo_admin"
	// CapWorkRead is a read of work items, queries and boards, including the
	// two query POSTs and work-item search.
	CapWorkRead Capability = "work_read"
	// CapWorkWrite is creating and updating work items, including through the
	// batch door.
	CapWorkWrite Capability = "work_write"
	// CapWorkAdmin is deleting or destroying work items and changing the
	// organisation's area and iteration paths, fields and tags. Azure DevOps
	// serves all of it from vso.work_write; only its permission model tells it
	// apart from editing, so the classifier does too.
	CapWorkAdmin Capability = "work_admin"
	// CapWikiRead is a read of wikis, including wiki search.
	CapWikiRead Capability = "wiki_read"
	// CapWikiWrite is writing a wiki.
	CapWikiWrite Capability = "wiki_write"
	// CapBuildRead is a read of builds and pipelines.
	CapBuildRead Capability = "build_read"
	// CapBuildExecute is queueing, cancelling or updating a pipeline run.
	CapBuildExecute Capability = "build_execute"
	// CapBuildAdmin is editing a pipeline definition — deciding what every
	// future run executes.
	CapBuildAdmin Capability = "build_admin"
	// CapReleaseRead is a read of classic releases and release pipelines.
	CapReleaseRead Capability = "release_read"
	// CapReleaseExecute is creating, deploying and deleting a release.
	CapReleaseExecute Capability = "release_execute"
	// CapReleaseAdmin is editing a release pipeline or answering a release
	// approval — deciding what every future release deploys.
	CapReleaseAdmin Capability = "release_admin"
	// CapServiceEndpointRead is a read of service connections.
	CapServiceEndpointRead Capability = "serviceendpoint_read"
	// CapServiceEndpointAdmin is changing a service connection — the object
	// that holds someone else's cloud credential.
	CapServiceEndpointAdmin Capability = "serviceendpoint_admin"
	// CapLibraryRead is a read of variable groups and secure files.
	CapLibraryRead Capability = "library_read"
	// CapPackagingRead is a read of feeds and packages, including a package
	// client's download.
	CapPackagingRead Capability = "packaging_read"
	// CapPackagingWrite is publishing, promoting, deprecating or unlisting a
	// package version.
	CapPackagingWrite Capability = "packaging_write"
	// CapPackagingManage is deleting or unpublishing a package version and
	// creating, changing or deleting a feed — Azure DevOps serves these only
	// under vso.packaging_manage.
	CapPackagingManage Capability = "packaging_manage"
	// CapTestRead is a read of test plans, runs and results.
	CapTestRead Capability = "test_read"
	// CapProjectRead is a read of projects and teams, and the signed-in
	// person's own profile and organisations.
	CapProjectRead Capability = "project_read"
	// CapIdentityRead is a read of the organisation's users, groups,
	// entitlements and directory identities.
	CapIdentityRead Capability = "identity_read"
	// CapProjectAdmin is creating, changing or deleting a project.
	CapProjectAdmin Capability = "project_admin"
	// CapSecurityAdmin is changing permissions, ACLs or directory identities.
	CapSecurityAdmin Capability = "security_admin"
)

// CapDiscovery is Azure DevOps' own API discovery: OPTIONS location
// discovery, connectiondata and resourceareas. It returns route templates,
// not organisation data, and Azure DevOps gates it on no scope. It is NOT
// grantable — no list may name it — and Permits allows it to any run holding
// at least one grantable capability, so no row has to be a floor.
const CapDiscovery Capability = "discovery"

// The REFUSED capabilities: Capability values, not errors, so a refusal can
// name the area it refused — an operator reading "denied_tokens" learns more
// than "refused" would — but none is grantable, so a caller refuses every one.
const (
	// CapDeniedTokens is the PAT and token-administration area: a lane that
	// can mint or revoke tokens is a lane that can leave the lane.
	CapDeniedTokens Capability = "denied_tokens"
	// CapDeniedServiceHooks is the service-hook area: forwarding org events
	// to a caller-chosen address is exfiltration with a webhook's paperwork.
	CapDeniedServiceHooks Capability = "denied_service_hooks"
	// CapDeniedExtensions is extension management: installing an extension
	// runs someone else's code inside the organisation.
	CapDeniedExtensions Capability = "denied_extension_management"
	// CapDeniedInternal is the undocumented contribution API the web UI
	// drives itself with (_apis/Contribution/…) — a single door onto every
	// other area, so no capability over it could mean anything.
	CapDeniedInternal Capability = "denied_internal"
	// CapUnclassifiedWrite is a write this catalogue does not recognize. It is
	// the fail-closed answer, not a capability anyone can hold.
	CapUnclassifiedWrite Capability = "unclassified_write"
	// CapUnclassifiedRead is a read outside readAreas, refused like its write
	// twin rather than classified as a read that 403s at the forge.
	CapUnclassifiedRead Capability = "unclassified_read"
)

// grantableCapabilities is the closed grantable set — the only values a
// ceiling, a profile or a scope request may name.
var grantableCapabilities = map[Capability]bool{
	CapCodeRead: true, CapCodeWrite: true, CapPR: true, CapPolicyAdmin: true,
	CapPolicyBypass: true, CapRepoAdmin: true,
	CapWorkRead: true, CapWorkWrite: true, CapWorkAdmin: true,
	CapWikiRead: true, CapWikiWrite: true,
	CapBuildRead: true, CapBuildExecute: true, CapBuildAdmin: true,
	CapReleaseRead: true, CapReleaseExecute: true, CapReleaseAdmin: true,
	CapServiceEndpointRead: true, CapServiceEndpointAdmin: true, CapLibraryRead: true,
	CapPackagingRead: true, CapPackagingWrite: true, CapPackagingManage: true,
	CapTestRead:    true,
	CapProjectRead: true, CapIdentityRead: true,
	CapProjectAdmin: true, CapSecurityAdmin: true,
}

// deniedCapabilities is the closed refused set.
var deniedCapabilities = map[Capability]bool{
	CapDeniedTokens: true, CapDeniedServiceHooks: true,
	CapDeniedExtensions: true, CapDeniedInternal: true,
}

// Grantable reports whether c may be written on a provider row and turned
// into a scope. Everything else is refused — test THIS, not enumerate
// refusals.
func (c Capability) Grantable() bool { return grantableCapabilities[c] }

// Denied reports whether c names an area no capability can be held over.
func (c Capability) Denied() bool { return deniedCapabilities[c] }

// Valid reports whether c is a value Classify can return at all.
func (c Capability) Valid() bool {
	return c.Grantable() || c.Denied() || c == CapDiscovery || c == CapUnclassifiedWrite || c == CapUnclassifiedRead
}

// GrantableCapabilities is the grantable set in a stable order, for the
// "want one of: …" half of a rejected write and for a UI's picker.
func GrantableCapabilities() []Capability {
	return slices.Sorted(maps.Keys(grantableCapabilities))
}

// GrantableCapabilityList is GrantableCapabilities as a comma-separated
// string, the shape an error message wants.
func GrantableCapabilityList() string {
	caps := GrantableCapabilities()
	strs := make([]string, len(caps))
	for i, c := range caps {
		strs[i] = string(c)
	}
	return strings.Join(strs, ", ")
}

// labels are the plain-language rendering of every capability, in the SECOND
// person ("this run may …"). Kept here, not in a console, so wording can't
// drift into a second definition of what a capability permits.
var labels = map[Capability]string{
	CapCodeRead:             "Clone, fetch and browse repositories, commits, branches, pull requests and branch policies, and search code",
	CapCodeWrite:            "Push commits and create or move branches, inside this run's own branch unless its policy allows any branch",
	CapPR:                   "Open, review and complete pull requests",
	CapPolicyAdmin:          "Change the branch policies themselves",
	CapPolicyBypass:         "Complete a pull request without its required reviewers or checks",
	CapRepoAdmin:            "Create, rename and delete repositories",
	CapWorkRead:             "Read work items, queries, boards, backlogs, areas and iterations, and search work items",
	CapWorkWrite:            "Create and update work items",
	CapWorkAdmin:            "Delete, restore or permanently destroy work items, and change area and iteration paths, fields and tags for everyone",
	CapWikiRead:             "Read wiki pages, their history and attachments, and search wikis",
	CapWikiWrite:            "Write wiki pages",
	CapBuildRead:            "Read pipelines, runs, builds, logs and artifacts",
	CapBuildExecute:         "Queue, cancel or update a pipeline run",
	CapBuildAdmin:           "Change pipeline definitions — what every future run executes",
	CapReleaseRead:          "Read classic release pipelines, releases and their stages",
	CapReleaseExecute:       "Create releases, deploy them to a stage, and delete them",
	CapReleaseAdmin:         "Change release pipelines and answer release approvals — what every future release deploys",
	CapServiceEndpointRead:  "Read service connection names, types and settings",
	CapServiceEndpointAdmin: "Change service connections and the credentials they hold",
	CapLibraryRead:          "Read variable groups and secure-file details",
	CapPackagingRead:        "List feeds, and download or restore packages",
	CapPackagingWrite:       "Publish packages to feeds",
	CapPackagingManage:      "Delete or unpublish package versions, and create, change or delete feeds, views and their permissions",
	CapTestRead:             "Read test plans, suites, cases, runs and results",
	CapProjectRead:          "Read projects, teams and your own profile",
	CapIdentityRead:         "Read the organisation's users, groups, memberships and licences, and directory identities",
	CapProjectAdmin:         "Create, change and delete projects",
	CapSecurityAdmin:        "Change permissions and identities",

	CapDiscovery: "Find where Azure DevOps serves each API — no organisation data",

	CapDeniedTokens:       "Not available: minting or revoking access tokens",
	CapDeniedServiceHooks: "Not available: forwarding organisation events to another address",
	CapDeniedExtensions:   "Not available: installing extensions into the organisation",
	CapDeniedInternal:     "Not available: the organisation's internal web API",
	CapUnclassifiedWrite:  "Not available: a write this deployment does not recognize",
	CapUnclassifiedRead:   "Not available: a read this deployment does not recognize",
}

// shortLabels are the console's canon short names (the owner-approved Azure
// DevOps access mock), for a Go-authored sentence a person reads beside that
// console — a launch refusal naming the capability. ADO_CAP_COPY in
// ui/src/app/lib/workspace-providers-copy.ts carries the same names, and
// ui/src/app/lib/ado-access-copy.test.ts pins the two together.
var shortLabels = map[Capability]string{
	CapCodeRead:             "Read code",
	CapCodeWrite:            "Push to the run's own branch",
	CapPR:                   "Contribute to pull requests",
	CapPolicyAdmin:          "Edit branch policies",
	CapPolicyBypass:         "Bypass policies when completing pull requests",
	CapRepoAdmin:            "Create, rename and delete repositories",
	CapWorkRead:             "View work items",
	CapWorkWrite:            "Edit work items",
	CapWorkAdmin:            "Delete work items & manage work tracking",
	CapWikiRead:             "Read wikis",
	CapWikiWrite:            "Edit wikis",
	CapBuildRead:            "View builds & pipelines",
	CapBuildExecute:         "Queue builds",
	CapBuildAdmin:           "Edit build pipelines",
	CapReleaseRead:          "View releases",
	CapReleaseExecute:       "Create releases & deploy",
	CapReleaseAdmin:         "Edit release pipelines",
	CapServiceEndpointRead:  "View service connections",
	CapServiceEndpointAdmin: "Manage service connections",
	CapLibraryRead:          "View variable groups & secure files",
	CapPackagingRead:        "Read feeds & packages",
	CapPackagingWrite:       "Publish packages",
	CapPackagingManage:      "Delete packages & manage feeds",
	CapTestRead:             "View test plans & results",
	CapProjectRead:          "View projects & teams",
	CapIdentityRead:         "Read users & groups",
	CapProjectAdmin:         "Manage projects & teams",
	CapSecurityAdmin:        "Manage permissions & identities",
}

// ShortLabel is c's canon short name, or its wire name for a value outside
// the grantable set.
func ShortLabel(c Capability) string {
	if l, ok := shortLabels[c]; ok {
		return l
	}
	return string(c)
}

// Label is c's plain-language rendering, or "" for a value outside the set —
// never a guess, so an unrecognized capability can't render as understood.
func Label(c Capability) string { return labels[c] }

// ProfileDefault is the profile a provider row with an empty default_profile
// gets: read the code and see the projects. Anything wider is a row's own
// choice, written out on the row.
func ProfileDefault() []Capability { return []Capability{CapProjectRead, CapCodeRead} }
