// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// repoURLUserinfo400 is the one sentence every door answers an http(s)
// repository URL carrying userinfo with. A user name alone is refused too: some
// hosts take a token in that position, and nothing here can tell a token from a
// name. The scp form (git@host:path) and ssh:// carry the SSH login user, never
// a secret, and stay allowed. The URL itself is never echoed.
const repoURLUserinfo400 = "a repository URL may not carry a user name, password or token before its host; give the plain URL and hand the credential over as a git_pat grant"

var errRepoURLUserinfo = errors.New(repoURLUserinfo400)

// repoURLHasUserinfo reports whether raw carries a credential before its host:
// an http(s) URL with anything before an '@' in its authority, other than the
// Azure DevOps Clone-button name, or an ssh:// URL whose userinfo holds a ':'
// (a password; the SSH login user alone is no secret).
func repoURLHasUserinfo(raw string) bool {
	// Format characters (a zero-width space) are invisible and dropped first, so
	// one cannot hide a credentialed URL's scheme.
	raw = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, raw)
	scheme, rest, ok := strings.Cut(strings.TrimSpace(raw), "://")
	if !ok {
		return false
	}
	authority, path := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority, path = rest[:i], rest[i:]
	}
	at := strings.LastIndex(authority, "@")
	switch {
	case at < 0:
		return false
	case strings.EqualFold(scheme, "ssh"):
		return strings.Contains(authority[:at], ":")
	case strings.EqualFold(scheme, "https") || strings.EqualFold(scheme, "http"):
		return !adoCloneButtonName(authority[:at], authority[at+1:], path)
	}
	return false
}

// adoCloneButtonName reports whether userinfo is the one name allowed: Azure
// DevOps' Clone button and its remoteUrl spell https://<org>@dev.azure.com/<org>/...,
// a user name (no password, not even an empty one) that repeats the organisation
// already in the path, so it hides nothing, over a path with no dot segment or
// escape. A token in that place does not.
func adoCloneButtonName(userinfo, host, path string) bool {
	org, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return strings.EqualFold(host, "dev.azure.com") && strings.HasPrefix(path, "/") &&
		userinfo != "" && !strings.Contains(userinfo, ":") && strings.EqualFold(userinfo, org) &&
		// git's curl squashes dot segments, so /<token>/../<org>/... would send the
		// token while the first segment matched it.
		repoLocatorPathSafe("https://"+host+path, nil)
}

// redactRepoURL is raw without the userinfo repoURLHasUserinfo flags, for audit
// rows, log lines and reads; anything else comes back unchanged. Rows stored before the doors
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

// redactWorkspaceRepoList is repos with redactRepoURL over each repo, copied
// only when one changes: a read must not reach back into a stored row.
func redactWorkspaceRepoList(repos []types.WorkspaceRepo) []types.WorkspaceRepo {
	for i := range repos {
		if repoURLHasUserinfo(repos[i].Repo) {
			repos = slices.Clone(repos)
			for j := i; j < len(repos); j++ {
				repos[j].Repo = redactRepoURL(repos[j].Repo)
			}
			return repos
		}
	}
	return repos
}

// redactRepoURLsInJSON is raw with redactRepoURL over every string value that
// carries a credential, for the stored documents a read serves whole (a
// preset's request, a template, a launch intent). raw comes back as it was
// when nothing changes or it does not decode.
func redactRepoURLsInJSON(raw []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // no float rounding on the way back out
	var v any
	if dec.Decode(&v) != nil {
		return raw
	}
	changed := false
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case string:
			if repoURLHasUserinfo(t) {
				changed = true
				return redactRepoURL(t)
			}
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		case map[string]any:
			for k := range t {
				t[k] = walk(t[k])
			}
		}
		return x
	}
	v = walk(v)
	if !changed {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}
