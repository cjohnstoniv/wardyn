// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ghscope

import (
	"net/http"
	"slices"
)

// The routes under /repos/{owner}/{repo}. A read is classified by its AREA
// (the first segment after the repository), a write by its area, method and
// position. DEFAULT is unclassified, so an area nobody added here — pages,
// deployments, environments, checks, statuses, variables, forks, dispatches,
// transfer — is refused.

// metadataAreas are the reads GitHub serves under the metadata permission.
var metadataAreas = []string{"collaborators", "contributors", "languages", "license", "rules", "rulesets", "tags", "topics"}

// adminWriteAreas are the areas whose every write changes who may reach the
// repository or what it enforces.
var adminWriteAreas = []string{"collaborators", "rulesets", "topics"}

// contentWriteAreas are the areas whose every write changes the repository's
// git data: the git database (refs and objects), files, merges, fork sync,
// releases (a release creates or moves a tag) and source imports.
var contentWriteAreas = []string{"contents", "git", "import", "merge-upstream", "merges", "releases"}

// codeReadAreas are the reads of the repository's content and history.
var codeReadAreas = []string{"activity", "branches", "commits", "compare", "contents", "git", "readme", "releases", "tarball", "zipball"}

// issueAreas are the issue tracker. Any read of one is CapIssuesRead and any
// write CapIssuesWrite.
var issueAreas = []string{"assignees", "issues", "labels", "milestones"}

func repoCapability(read bool, method string, sub []string) Capability {
	if len(sub) == 0 {
		switch {
		case read:
			return CapMetadata
		case method == http.MethodPatch || method == http.MethodDelete:
			return CapRepoAdmin
		}
		return CapUnclassifiedWrite
	}
	if c, ok := deniedRepoArea(sub); ok {
		return c
	}
	area := sub[0]
	switch {
	case area == "branches" && len(sub) > 2 && slices.Contains(sub[2:], "protection"):
		// Branch protection is served only under the administration
		// permission, reads included. A branch name may hold slashes, so the
		// resource is found anywhere after the name's first segment; a branch
		// whose own name has a "protection" segment reads as protection,
		// which asks for more, never less.
		return CapRepoAdmin
	case area == "pulls":
		return pullsCapability(read, method, sub)
	case area == "actions":
		return actionsCapability(read, method, sub)
	case slices.Contains(issueAreas, area):
		if read {
			return CapIssuesRead
		}
		return CapIssuesWrite
	case read && slices.Contains(metadataAreas, area):
		return CapMetadata
	case read && slices.Contains(codeReadAreas, area):
		return CapCodeRead
	case read:
		return CapUnclassifiedRead
	case slices.Contains(adminWriteAreas, area):
		return CapRepoAdmin
	case slices.Contains(contentWriteAreas, area), area == "branches" && len(sub) > 2 && sub[len(sub)-1] == "rename",
		area == "code-scanning" && at(sub, 1) == "alerts" && slices.Contains(sub, "autofix"):
		return CapRepoContentWriteREST
	}
	return CapUnclassifiedWrite
}

// deniedRepoArea is the repository half of the refusal table, checked BEFORE
// the method. Deploy keys and runner registration are token doors: each
// hands out a credential that outlives the run.
func deniedRepoArea(sub []string) (Capability, bool) {
	switch a := at(sub, 0); {
	case a == "hooks":
		return CapDeniedHooks, true
	case a == "keys", a == "actions" && at(sub, 1) == "runners":
		return CapDeniedTokens, true
	case slices.Contains(secretAreas, a) && at(sub, 1) == "secrets",
		a == "actions" && at(sub, 1) == "organization-secrets",
		a == "environments" && at(sub, 2) == "secrets":
		return CapDeniedSecrets, true
	}
	return "", false
}

// pullParts are the resources under one pull request this catalogue reads,
// and the ones it writes. A codespace opened on a pull request is absent from
// both on purpose.
var (
	pullReadParts  = []string{"comments", "commits", "files", "merge", "requested_reviewers", "reviews"}
	pullWriteParts = []string{"comments", "requested_reviewers", "reviews"}
	// pullContentParts write the repository: merging a pull request, and
	// updating its branch from the base.
	pullContentParts = []string{"merge", "merge-async", "update-branch"}
)

// pullsCapability is the pulls area: reading pull requests is CapCodeRead,
// a merge or branch update CapRepoContentWriteREST, every other write CapPR.
// POSITIONAL: sub[1] is a pull request's number or the literal review-comment
// collection, and nothing else.
func pullsCapability(read bool, method string, sub []string) Capability {
	c := CapPR
	if read {
		c = CapCodeRead
	}
	switch {
	case len(sub) == 1:
		if read || method == http.MethodPost {
			return c
		}
	case sub[1] == "comments":
		if read || len(sub) > 2 {
			return c
		}
	case !isNumber(sub[1]):
	case len(sub) == 2:
		if read || method == http.MethodPatch {
			return c
		}
	case !read && slices.Contains(pullContentParts, sub[2]):
		return CapRepoContentWriteREST
	case read && slices.Contains(pullReadParts, sub[2]), !read && slices.Contains(pullWriteParts, sub[2]):
		return c
	}
	return unclassified(read)
}

// actionsWrites are the actions area's classified writes, keyed by method and
// shape: the resource, then "{id}" for the one segment that names an object,
// then the action on it.
var actionsWrites = map[string]Capability{
	"POST runs/{id}/approve":           CapActionsAdmin,
	"POST runs/{id}/cancel":            CapActionsExecute,
	"POST runs/{id}/force-cancel":      CapActionsExecute,
	"POST runs/{id}/rerun":             CapActionsExecute,
	"POST runs/{id}/rerun-failed-jobs": CapActionsExecute,
	"DELETE runs/{id}":                 CapActionsAdmin,
	"DELETE runs/{id}/logs":            CapActionsAdmin,
	"POST jobs/{id}/rerun":             CapActionsExecute,
	"POST workflows/{id}/dispatches":   CapActionsExecute,
	"PUT workflows/{id}/enable":        CapActionsAdmin,
	"PUT workflows/{id}/disable":       CapActionsAdmin,
	"DELETE artifacts/{id}":            CapActionsAdmin,
	"DELETE caches":                    CapActionsAdmin,
	"DELETE caches/{id}":               CapActionsAdmin,
}

// actionsReadResources are the actions resources a read is classified in.
var actionsReadResources = []string{"artifacts", "cache", "caches", "jobs", "runs", "workflows"}

// actionsCapability is the actions area, limited to runs, jobs, workflows,
// artifacts and caches. Its secrets and runners are refused by
// deniedRepoArea, a write to its permissions is CapRepoAdmin (GitHub serves
// it under administration), and its variables and OIDC settings, each under
// a permission of its own, are unclassified.
func actionsCapability(read bool, method string, sub []string) Capability {
	res := at(sub, 1)
	switch {
	case res == "permissions" && !read:
		return CapRepoAdmin
	case !slices.Contains(actionsReadResources, res):
		return unclassified(read)
	case read:
		return CapActionsRead
	case len(sub) > 4:
		return CapUnclassifiedWrite
	}
	key := method + " " + res
	if len(sub) > 2 {
		key += "/{id}"
	}
	if len(sub) > 3 {
		key += "/" + sub[3]
	}
	if c, ok := actionsWrites[key]; ok {
		return c
	}
	return CapUnclassifiedWrite
}

// isNumber reports whether s is all ASCII digits.
func isNumber(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}
