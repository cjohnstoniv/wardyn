// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// repoURLUserinfo400 is the one sentence every door answers an http(s)
// repository URL carrying userinfo with. A user name alone is refused too: some
// hosts take a token in that position, and nothing here can tell a token from a
// name. The scp form (git@host:path) and ssh:// carry the SSH login user, never
// a secret, and stay allowed. The URL itself is never echoed.
const repoURLUserinfo400 = "a repository URL may not carry a user name, password or token before its host; give the plain URL and hand the credential over as a git_pat grant"

var errRepoURLUserinfo = errors.New(repoURLUserinfo400)

// repoURLHasUserinfo reports whether raw is an http(s) URL with anything before
// an '@' in its authority, other than the Azure DevOps Clone-button name.
func repoURLHasUserinfo(raw string) bool {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(raw), "://")
	if !ok || !(strings.EqualFold(scheme, "https") || strings.EqualFold(scheme, "http")) {
		return false
	}
	authority, path := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority, path = rest[:i], rest[i:]
	}
	at := strings.LastIndex(authority, "@")
	return at >= 0 && !adoCloneButtonName(authority[:at], authority[at+1:], path)
}

// adoCloneButtonName reports whether userinfo is the one name allowed: Azure
// DevOps' Clone button and its remoteUrl spell https://<org>@dev.azure.com/<org>/...,
// a user name (no password, not even an empty one) that repeats the organisation
// already in the path, so it hides nothing. A token in that place does not.
func adoCloneButtonName(userinfo, host, path string) bool {
	org, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return strings.EqualFold(host, "dev.azure.com") && strings.HasPrefix(path, "/") &&
		userinfo != "" && !strings.Contains(userinfo, ":") && strings.EqualFold(userinfo, org)
}

// redactRepoURL is raw without an http(s) URL's userinfo, for audit rows and
// log lines; anything else comes back unchanged. Rows stored before the doors
// refused userinfo still reach those sinks.
func redactRepoURL(raw string) string {
	if !repoURLHasUserinfo(raw) {
		return raw
	}
	return repoRecordURL(strings.TrimSpace(raw))
}

// repoURLUserinfoRefusal is the 400 for repos when any carries userinfo, else
// nil. The admission gate asks it before anything else.
func repoURLUserinfoRefusal(repos ...string) *runRefusal {
	for _, repo := range repos {
		if repoURLHasUserinfo(repo) {
			return runError(http.StatusBadRequest, reasonRepoURLCredentials, repoURLUserinfo400)
		}
	}
	return nil
}

// repoURLUserinfoShapeRefusal holds a run request's own repository fields to the
// rule, for the doors that store a request without admitting it (launch presets,
// templates) as well as launch: repo, devcontainer_repo, and the workspace_repos
// of inline_policy and policy_overlay.
func repoURLUserinfoShapeRefusal(req createRunRequest) *runRefusal {
	repos := []string{req.Repo, req.DevcontainerRepo}
	if req.InlinePolicy != nil {
		repos = append(repos, repoLocatorsOf(req.InlinePolicy.WorkspaceRepos)...)
	}
	var overlay struct {
		WorkspaceRepos []types.WorkspaceRepo `json:"workspace_repos"`
	}
	if json.Unmarshal(req.PolicyOverlay, &overlay) == nil {
		repos = append(repos, repoLocatorsOf(overlay.WorkspaceRepos)...)
	}
	return repoURLUserinfoRefusal(repos...)
}

// repoURLMsgReason is bucket, or reasonRepoURLCredentials when msg is the
// userinfo refusal, for a validator that answers a message rather than an error.
func repoURLMsgReason(msg, bucket string) string {
	if strings.HasSuffix(msg, repoURLUserinfo400) {
		return reasonRepoURLCredentials
	}
	return bucket
}

// The read projections: a row stored before the doors refused userinfo is
// served with redactRepoURL over its repository URLs.

func redactRunRepos(runs []types.AgentRun) {
	for i := range runs {
		runs[i].Repo = redactRepoURL(runs[i].Repo)
	}
}

func redactWorkspaceRepoURLs(ws types.Workspace) types.Workspace {
	ws.Sources = slices.Clone(ws.Sources) // shared with the rest of the request
	for i := range ws.Sources {
		if ws.Sources[i].Type == types.WorkspaceSourceTypeRepo {
			ws.Sources[i].Source = redactRepoURL(ws.Sources[i].Source)
		}
	}
	ws.Source = redactRepoURL(ws.Source)
	return ws
}

func redactSourceRepoURLs(srcs []types.Source) {
	for i := range srcs {
		srcs[i].Locator = redactRepoURL(srcs[i].Locator)
	}
}
