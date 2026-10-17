// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package ghscope is the GitHub CAPABILITY CATALOGUE, the sibling of
// internal/adoscope: it names the access a request needs, classifies one
// request to exactly one name and the repository it targets, and maps those
// names to GitHub permissions — because a person's own GitHub token carries
// everything that person can reach in every repository, so the classification
// and the repository it names are the only things that can hold a run to the
// work it was launched for.
//
// The package is PURE, standard-library only, for adoscope's reasons
// (importing internal/types would cycle; internal/api or internal/egress
// would let a call site's own idea of "read" drift from the classifier's).
//
// FAIL-CLOSED IS THE CONTRACT: Classify errors on a request it can't reason
// about, and returns a non-grantable capability for a route it does not
// recognize; a caller refuses both. Permits is the one spelling of "allowed".
package ghscope

import (
	"maps"
	"slices"
	"strings"
)

// Capability is the ONE access name a classified request needs. The set is
// closed: a request is one of these or it is refused.
type Capability string

// The GRANTABLE capabilities — the only values a provider row's ceiling or
// profile, or a policy's github_capabilities, may name, and the only ones
// PermissionsFor turns into permissions.
const (
	// CapCodeRead is a read of a repository's contents, commits, branches,
	// releases and pull requests, and a clone or fetch.
	CapCodeRead Capability = "code_read"
	// CapCodeWrite is a git push. The REST routes that change content or refs
	// are not this capability: they are CapRepoContentWriteREST, refused.
	CapCodeWrite Capability = "code_write"
	// CapWorkflowsWrite is a push that changes a workflow file under
	// .github/workflows. GitHub refuses such a push from a token without its
	// separate workflows permission, so it is its own capability, and it means
	// nothing without CapCodeWrite beside it. The classifier cannot see which
	// files a push touches; GitHub's token check is what tells them apart.
	CapWorkflowsWrite Capability = "workflows_write"
	// CapPR is opening, updating and reviewing a pull request. Merging one,
	// or updating its branch, writes the repository and is
	// CapRepoContentWriteREST.
	CapPR Capability = "pr"
	// CapIssuesRead is a read of issues, their comments, labels and milestones.
	CapIssuesRead Capability = "issues_read"
	// CapIssuesWrite is creating and changing issues, comments, labels and
	// milestones. A pull request's conversation comment is an issue comment
	// on GitHub, so it is this capability, not CapPR.
	CapIssuesWrite Capability = "issues_write"
	// CapActionsRead is a read of workflows, runs, jobs, logs, artifacts and
	// caches.
	CapActionsRead Capability = "actions_read"
	// CapActionsExecute is dispatching, re-running or cancelling a workflow run.
	CapActionsExecute Capability = "actions_execute"
	// CapActionsAdmin is enabling or disabling a workflow, approving a held
	// run, and deleting runs, logs, artifacts and caches. GitHub serves it
	// from the same permission as CapActionsExecute; only the classifier
	// tells them apart.
	CapActionsAdmin Capability = "actions_admin"
	// CapPackagesRead is a read of the packages an organisation or account
	// owns.
	CapPackagesRead Capability = "packages_read"
	// CapPackagesWrite is deleting or restoring a package or one of its versions.
	CapPackagesWrite Capability = "packages_write"
	// CapRepoAdmin is changing or deleting a repository, its collaborators,
	// rulesets, branch protection and Actions settings, and creating one in an
	// organisation.
	CapRepoAdmin Capability = "repo_admin"
	// CapOrgRead is a read of an organisation's profile, members, teams and
	// repository list.
	CapOrgRead Capability = "org_read"
	// CapOrgAdmin is changing an organisation's settings, members, teams and
	// invitations.
	CapOrgAdmin Capability = "org_admin"
	// CapIdentity is reading the signed-in person's own profile (GET /user) —
	// what most tools call first to learn who they are. Not in ProfileDefault.
	CapIdentity Capability = "identity"
)

// CapMetadata is a read GitHub serves under the metadata permission every
// token holds: a repository's own description, languages, topics, tags and
// rulesets, and the API's root and rate limit. It is NOT grantable — no list
// may name it — and Permits allows it to any run holding at least one
// grantable capability, still held to the run's repositories.
const CapMetadata Capability = "metadata"

// The REFUSED capabilities: Capability values, not errors, so a refusal can
// name the area it refused, but none is grantable, so a caller refuses every one.
const (
	// CapDeniedTokens is every door onto a second credential: app and
	// installation tokens, OAuth authorizations, deploy keys, and the
	// registration tokens of self-hosted runners. A lane that can obtain one
	// is a lane that can leave the lane.
	CapDeniedTokens Capability = "denied_tokens"
	// CapDeniedHooks is repository and organisation webhooks: forwarding
	// events to a caller-chosen address is exfiltration with a webhook's
	// paperwork.
	CapDeniedHooks Capability = "denied_hooks"
	// CapDeniedSecrets is Actions, Dependabot, Codespaces and environment
	// secrets, and private registry credentials. GitHub serves them under
	// permissions of their own (secrets, organization_secrets, …), which
	// PermissionsFor never asks for.
	CapDeniedSecrets Capability = "denied_secrets"
	// CapDeniedAccount is everything that spans the signed-in person's whole
	// reach rather than this run's repositories: everything under /user but
	// the profile itself, the numeric /repositories aliases, notifications
	// and gists.
	CapDeniedAccount Capability = "denied_account"
	// CapDeniedSearch is the search API: a search runs across everything the
	// token can see, and its query, not its path, says where.
	CapDeniedSearch Capability = "denied_search"
	// CapDeniedGraphQL is the GraphQL endpoint — a single door onto every
	// other area, so no capability over it could mean anything.
	CapDeniedGraphQL Capability = "denied_graphql"
	// CapRepoContentWriteREST is a REST write to the repository's git data:
	// creating, moving or deleting a ref, creating a git object, writing or
	// deleting a file, merging (a branch, an upstream, a pull request),
	// updating a pull request's branch, renaming a branch, a release (which
	// creates or moves a tag), a source import, and an autofix commit. Such a
	// request reaches GitHub with no Wardyn check on which ref it moves or
	// what it writes — the run's branch confinement and push content rules
	// apply to git pushes, not to these routes — so it is refused, whatever
	// the run holds. Repository content reaches GitHub through git push.
	CapRepoContentWriteREST Capability = "repo_content_write_rest"
	// CapUnclassifiedWrite is a write this catalogue does not recognize. It is
	// the fail-closed answer, not a capability anyone can hold.
	CapUnclassifiedWrite Capability = "unclassified_write"
	// CapUnclassifiedRead is a read this catalogue does not recognize, refused
	// like its write twin rather than classified as a read GitHub might serve.
	CapUnclassifiedRead Capability = "unclassified_read"
)

// grantableCapabilities is the closed grantable set.
var grantableCapabilities = map[Capability]bool{
	CapCodeRead: true, CapCodeWrite: true, CapWorkflowsWrite: true, CapPR: true,
	CapIssuesRead: true, CapIssuesWrite: true,
	CapActionsRead: true, CapActionsExecute: true, CapActionsAdmin: true,
	CapPackagesRead: true, CapPackagesWrite: true,
	CapRepoAdmin: true, CapOrgRead: true, CapOrgAdmin: true, CapIdentity: true,
}

// deniedCapabilities is the closed refused set.
var deniedCapabilities = map[Capability]bool{
	CapDeniedTokens: true, CapDeniedHooks: true, CapDeniedSecrets: true, CapDeniedAccount: true,
	CapDeniedSearch: true, CapDeniedGraphQL: true, CapRepoContentWriteREST: true,
}

// Grantable reports whether c may be written on a provider row or a policy and
// turned into a permission. Everything else is refused — test THIS, not
// enumerate refusals.
func (c Capability) Grantable() bool { return grantableCapabilities[c] }

// Denied reports whether c names an area no capability can be held over.
func (c Capability) Denied() bool { return deniedCapabilities[c] }

// Valid reports whether c is a value Classify can return at all.
func (c Capability) Valid() bool {
	return c.Grantable() || c.Denied() || c == CapMetadata || c == CapUnclassifiedWrite || c == CapUnclassifiedRead
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

// labels are the plain-language rendering of every capability. Kept here, not
// in a console, so wording can't drift into a second definition of what a
// capability permits.
var labels = map[Capability]string{
	CapCodeRead:       "Clone, fetch and browse repositories, commits, branches, releases and pull requests",
	CapCodeWrite:      "Push commits with git, inside this run's own branch unless its policy allows any branch",
	CapWorkflowsWrite: "Push changes to workflow files under .github/workflows",
	CapPR:             "Open, update and review pull requests",
	CapIssuesRead:     "Read issues, comments, labels and milestones",
	CapIssuesWrite:    "Create and change issues, comments, labels and milestones",
	CapActionsRead:    "Read workflows, runs, jobs, logs and artifacts",
	CapActionsExecute: "Dispatch, re-run or cancel a workflow run",
	CapActionsAdmin:   "Enable or disable workflows, approve held runs, and delete runs, logs, artifacts and caches",
	CapPackagesRead:   "Read the packages of the organisations that own this run's repositories",
	CapPackagesWrite:  "Delete or restore those packages and their versions",
	CapRepoAdmin:      "Change or delete repositories, their collaborators, rulesets, branch protection and Actions settings, and create repositories",
	CapOrgRead:        "Read the organisation's profile, members, teams and repository list",
	CapOrgAdmin:       "Change the organisation's settings, members, teams and invitations",
	CapIdentity:       "Read your own GitHub profile",

	CapMetadata: "Read a repository's own description, languages, topics, tags and rulesets",

	CapDeniedTokens:         "Not available: minting, listing or revoking access tokens and keys",
	CapDeniedHooks:          "Not available: forwarding repository or organisation events to another address",
	CapDeniedSecrets:        "Not available: repository, organisation and environment secrets",
	CapDeniedAccount:        "Not available: your account's keys, emails, gists, notifications and repositories outside this run",
	CapDeniedSearch:         "Not available: searching across GitHub",
	CapDeniedGraphQL:        "Not available: the GraphQL API — this lane is REST only",
	CapRepoContentWriteREST: "Not available: changing files, branches, tags or merges through the API — push with git instead",
	CapUnclassifiedWrite:    "Not available: a write this deployment does not recognize",
	CapUnclassifiedRead:     "Not available: a read this deployment does not recognize",
}

// Label is c's plain-language rendering, or "" for a value outside the set —
// never a guess, so an unrecognized capability can't render as understood.
func Label(c Capability) string { return labels[c] }

// ProfileDefault is the profile a provider row with an empty default_profile
// gets: read the code. Anything wider is a row's own choice, written out on
// the row.
func ProfileDefault() []Capability { return []Capability{CapCodeRead} }
