// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ghscope

import (
	"fmt"
	"slices"
	"strings"
)

// The two access levels every permission in capabilityPermissions takes.
// GitHub knows a third, admin, on a few permissions this catalogue never asks for.
const (
	levelRead  = "read"
	levelWrite = "write"
)

// capabilityPermissions is the capability -> GitHub permission table, and THE
// ONE PLACE a capability becomes a permission. Each row is the COMBINATION the
// capability's routes need, not one permission per capability: reading pull
// requests is served under pull_requests, so code_read carries it beside
// contents; a push that changes a workflow file needs workflows on top of
// contents, so workflows_write carries both. The keys are the ones GitHub's
// "create an installation access token" request takes — repository
// permissions (contents, workflows, pull_requests, issues, actions, packages,
// administration) and organisation permissions (members,
// organization_administration) — each at read or write, the levels it lists
// for them.
//
// Permissions no row names are deliberate: secrets, organization_secrets,
// repository_hooks, organization_hooks and the rest of the denied areas'
// permissions are never asked for. CapIdentity names none: reading one's own
// profile needs no permission.
//
// SEVERAL CAPABILITIES SHARE ONE PERMISSION (CapActionsExecute and
// CapActionsAdmin both need actions: write), so a token minted from this table
// is wider than either; the classifier, not the token, tells them apart.
var capabilityPermissions = map[Capability]map[string]string{
	CapCodeRead:       {"contents": levelRead, "pull_requests": levelRead},
	CapCodeWrite:      {"contents": levelWrite},
	CapWorkflowsWrite: {"contents": levelWrite, "workflows": levelWrite},
	CapPR:             {"pull_requests": levelWrite},
	CapIssuesRead:     {"issues": levelRead},
	CapIssuesWrite:    {"issues": levelWrite},
	CapActionsRead:    {"actions": levelRead},
	CapActionsExecute: {"actions": levelWrite},
	CapActionsAdmin:   {"actions": levelWrite},
	CapPackagesRead:   {"packages": levelRead},
	CapPackagesWrite:  {"packages": levelWrite},
	CapRepoAdmin:      {"administration": levelWrite},
	CapOrgRead:        {"members": levelRead},
	CapOrgAdmin:       {"members": levelWrite, "organization_administration": levelWrite},
	CapIdentity:       {},
}

// PermissionsFor is the GitHub permission set for caps: each key at the
// highest level any capability asks, and metadata: read always, which GitHub
// grants every token.
//
// SECURITY: it never returns an empty or nil map. GitHub reads a token request
// that names NO permissions as "every permission the installation holds", so
// an empty result handed to a minter would be the widest token there is, not
// the narrowest. A capability that is not grantable is an ERROR for the same
// reason ScopesFor's is in adoscope: an unclassified write must be refused,
// not minted for. A caller MUST treat a non-nil error as "refuse": the map it
// returns then is nil, and a nil permission map handed to a minter is the
// all-permissions request above.
func PermissionsFor(caps []Capability) (map[string]string, error) {
	out := map[string]string{"metadata": levelRead}
	for _, c := range caps {
		if !c.Grantable() {
			return nil, fmt.Errorf("ghscope: %q is not a grantable capability — it has no permissions and must be refused, not minted", c)
		}
		for perm, level := range capabilityPermissions[c] {
			if level == levelWrite || out[perm] == "" {
				out[perm] = level
			}
		}
	}
	return out, nil
}

// Permits is THE gate: may a run holding granted, launched for repos, perform
// v? The only spelling of "allowed" this package offers.
//
// SECURITY: it checks TWO things and a caller needs both. The capability: v's
// must be grantable and in granted. And the pin: a request that names a
// repository must name one of repos, and one that names only an organisation
// or account must name the owner of one of repos — a person's own token
// reaches every repository they can, so the path is the only place a run is
// held to its own. repos are "owner/name" as RepoKey reads them; an entry it
// cannot read matches nothing, and an empty list permits nothing that names a
// repository or an owner.
//
// METADATA is the one verdict no list names: any run holding at least one
// grantable capability may make it, inside the same pin.
func Permits(granted []Capability, repos []string, v Verdict) bool {
	if !pinned(repos, v) {
		return false
	}
	if v.Capability == CapMetadata {
		return slices.ContainsFunc(granted, Capability.Grantable)
	}
	return v.Capability.Grantable() && slices.Contains(granted, v.Capability)
}

// pinned reports whether what v names lies inside repos. A verdict that names
// neither a repository nor an owner (the API root, the person's own profile)
// has nothing to pin.
func pinned(repos []string, v Verdict) bool {
	if v.Repo == "" && v.Owner == "" {
		return true
	}
	return slices.ContainsFunc(repos, func(r string) bool {
		key, ok := RepoKey(r)
		if !ok {
			return false
		}
		if v.Repo != "" {
			return key == v.Repo
		}
		owner, _, _ := strings.Cut(key, "/")
		return owner == v.Owner
	})
}
