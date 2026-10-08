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

// actionsRunWrites are the writes on one workflow run, by the segment after
// its id.
var actionsRunWrites = map[string]Capability{
	"approve":           CapActionsAdmin,
	"cancel":            CapActionsExecute,
	"force-cancel":      CapActionsExecute,
	"rerun":             CapActionsExecute,
	"rerun-failed-jobs": CapActionsExecute,
}

// actionsCapability is the actions area, limited to runs, jobs, workflows,
// artifacts and caches. Its secrets and runners are refused by
// deniedRepoArea, a write to its permissions is CapRepoAdmin (GitHub serves
// it under administration), and its variables and OIDC settings, each under
// a permission of its own, are unclassified.
func actionsCapability(read bool, method string, sub []string) Capability {
	res, part := at(sub, 1), at(sub, 3)
	if res == "permissions" && !read {
		return CapRepoAdmin
	}
	if !slices.Contains([]string{"artifacts", "cache", "caches", "jobs", "runs", "workflows"}, res) {
		return unclassified(read)
	}
	if read {
		return CapActionsRead
	}
	del, post := method == http.MethodDelete, method == http.MethodPost
	switch {
	case res == "runs" && len(sub) == 4 && post:
		if c, ok := actionsRunWrites[part]; ok {
			return c
		}
	case res == "runs" && del && (len(sub) == 3 || len(sub) == 4 && part == "logs"):
		return CapActionsAdmin
	case res == "jobs" && len(sub) == 4 && post && part == "rerun":
		return CapActionsExecute
	case res == "workflows" && len(sub) == 4 && post && part == "dispatches":
		return CapActionsExecute
	case res == "workflows" && len(sub) == 4 && method == http.MethodPut && (part == "enable" || part == "disable"):
		return CapActionsAdmin
	case (res == "artifacts" || res == "caches") && del && len(sub) <= 3:
		return CapActionsAdmin
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
