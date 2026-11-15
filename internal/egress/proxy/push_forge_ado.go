// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// What a push leaves unchanged, read from Azure DevOps: forgeRepo's questions
// over the Git REST API, api-version 7.1, with the lane's own brokered
// credential and only the repository the push names.
//
// A receive-pack request is read before Azure DevOps has its objects, so a
// commit diff of the pushed commit does not exist yet. What does exist are the
// commits the push builds on, so the answer is the pack plus those base trees,
// exactly as on GitHub:
//
//	default branch   GET .../repositories/{repo}                     defaultBranch
//	a branch's tip   GET .../refs?filter=heads/{branch}              the exact name's objectId
//	"is in history"  GET .../diffs/commits (base and target by id)   commonCommit == base
//	root tree        GET .../commits/{id}                            treeId
//	one directory    GET .../trees/{id}                              treeEntries
//
// Every way an answer can fall short keeps the strict reading: a status other
// than 200 (Azure DevOps answers a sign-in page with a 203), a body that is not
// the JSON asked for or is over maxForgeBody, a continuation token on any read
// (the rest of the answer was never read), and maxForgeReads and forgeReadWait.
// A 400 or 404 from the diff means the server does not know the commit, which
// makes it no vouched base, and a carried commit it does not know is new.
//
// A branch moved other than forward stays unknown as well: against a base the
// move rewinds to, the pack says nothing about what the old tip held
// (fastForwards).

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/gitpack"
)

// adoClears says which rules may have Azure DevOps clear what a pack leaves out.
// Setting one false puts that rule back on the strict reading on this forge: every
// entry the pack does not carry refuses. A var so a test can show exactly that.
var adoClears = struct{ reviewPaths, denyPaths, maxFileSize, newExecutables, noObjects bool }{
	reviewPaths: true, denyPaths: true, maxFileSize: true, newExecutables: true, noObjects: true,
}

// forRule is f, or no reader at all where allowed is false on Azure DevOps.
func (f *forgeRepo) forRule(allowed bool) *forgeRepo {
	if f != nil && f.ado != nil && !allowed {
		return nil
	}
	return f
}

// adoForge is what makes a forgeRepo read Azure DevOps.
type adoForge struct {
	host string
	// base is ".../_apis/git/repositories/<repo>", the repository the push's own remote names.
	base string
	auth func(context.Context) (injectedHeader, error)
	hdr  injectedHeader
}

const (
	adoAPIVersion = "7.1"

	whyForced     = "this push moves a branch other than forward from the tip it states, so the commit it builds on does not show what the branch loses"
	whyHeldForced = "this push moves a branch other than forward from the tip it states, for: "
	whyCutShort   = "this push names more history than the broker follows, so the broker stopped reading and takes no branch as moved forward from the tip it states"
)

// adoRepoAPIBase is the REST path of the repository a receive-pack path names:
// <collection>/[<project>/]_git/<repo>/git-receive-pack, where the collection is
// the organisation on dev.azure.com, nothing (or DefaultCollection) on
// <org>.visualstudio.com and the grant's collection on a Server. The short form
// is the repository named after its project. Any other shape returns false.
func adoRepoAPIBase(host, rest, org string) (string, bool) {
	path, ok := strings.CutSuffix(rest, "/git-receive-pack")
	if !ok {
		return "", false
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	at := len(segs) - 2
	if at < 0 || !strings.EqualFold(segs[at], "_git") {
		return "", false
	}
	for i, s := range segs {
		// Azure DevOps refuses a project or repository name starting with an underscore.
		if s == "" || s == "." || s == ".." || i != at && strings.HasPrefix(s, "_") {
			return "", false
		}
	}
	depth := 1
	switch {
	case strings.HasSuffix(host, ".visualstudio.com"):
		depth = 0
		if strings.EqualFold(segs[0], "DefaultCollection") {
			depth = 1
		}
	case !adoHostedHost(host):
		depth = len(strings.Split(strings.Trim(org, "/"), "/"))
	}
	repo := segs[at+1]
	var parts []string
	switch at {
	case depth:
		parts = append(append(parts, segs[:at]...), repo)
	case depth + 1:
		parts = segs[:at]
	default:
		return "", false
	}
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return "/" + strings.Join(append(parts[:len(parts):len(parts)], "_apis", "git", "repositories", url.PathEscape(repo)), "/"), true
}

// adoEntraForge is the Azure DevOps Entra lane's reader; nil for a remote whose
// repository it cannot name, which keeps the strict reading.
func (p *Proxy) adoEntraForge(host, rest string, grant ADOGrant) *forgeRepo {
	base, ok := adoRepoAPIBase(host, rest, grant.Organization)
	if !ok {
		return nil
	}
	return &forgeRepo{p: p, ado: &adoForge{host: host, base: base, auth: func(ctx context.Context) (injectedHeader, error) {
		hdr, ok, err := p.inject.resolveCtx(ctx, host)
		if err != nil {
			return injectedHeader{}, err
		}
		if !ok {
			return injectedHeader{}, errors.New("no Azure DevOps credential is configured")
		}
		return hdr, nil
	}}}
}

// adoPATForge is the git_pat lane's reader for Azure DevOps Services.
func (p *Proxy) adoPATForge(host, rest string, g PATGrant) *forgeRepo {
	base, ok := adoRepoAPIBase(host, rest, "")
	if !ok {
		return nil
	}
	return &forgeRepo{p: p, ado: &adoForge{host: host, base: base, auth: func(ctx context.Context) (injectedHeader, error) {
		tok, user, err := p.patToken(ctx, g)
		if err != nil {
			return injectedHeader{}, err
		}
		return injectedHeader{name: "Authorization",
			value: "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+tok))}, nil
	}}}
}

// adoGet reads one REST path under the repository into v.
func (f *forgeRepo) adoGet(ctx context.Context, path string, v any) (int, error) {
	a := f.ado
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	status, hdr, err := f.read(ctx, a.host, a.base+path+sep+"api-version="+adoAPIVersion, func(ctx context.Context, req *http.Request) error {
		if a.hdr.value == "" {
			h, err := a.auth(ctx)
			if err != nil {
				return fmt.Errorf("the run's credential: %w", err)
			}
			registerHeaderCredentialFor(f.p.maskKey, h.value)
			a.hdr = h
		}
		req.Header.Set(a.hdr.name, a.hdr.value)
		req.Header.Set("Accept", "application/json")
		return nil
	}, v)
	if hdr.Get("x-ms-continuationtoken") != "" {
		return 0, errors.New("the forge answered with only part of a listing")
	}
	return status, err
}

// adoDefaultBranch is "" for a repository with no default branch (an empty one).
func (f *forgeRepo) adoDefaultBranch(ctx context.Context) (string, error) {
	var repo struct {
		DefaultBranch string `json:"defaultBranch"`
	}
	status, err := f.adoGet(ctx, "", &repo)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("reading the repository's default branch answered %d", status)
	}
	return strings.TrimPrefix(repo.DefaultBranch, "refs/heads/"), nil
}

// adoBranchTip reads ref's tip, "" when the forge holds no such branch. The
// filter is a prefix, so only the exact name counts.
func (f *forgeRepo) adoBranchTip(ctx context.Context, ref string) (string, error) {
	var refs struct {
		Value []struct {
			Name     string `json:"name"`
			ObjectID string `json:"objectId"`
		} `json:"value"`
	}
	filter := url.QueryEscape(strings.TrimPrefix(ref, "refs/"))
	status, err := f.adoGet(ctx, "/refs?filter="+filter, &refs)
	switch {
	case err != nil:
		return "", err
	case status == http.StatusNotFound:
		return "", nil
	case status != http.StatusOK:
		return "", fmt.Errorf("reading %s answered %d", ref, status)
	}
	for _, r := range refs.Value {
		if r.Name != ref {
			continue
		}
		if !isHex(r.ObjectID) {
			return "", fmt.Errorf("reading %s named no commit", ref)
		}
		return r.ObjectID, nil
	}
	return "", nil
}

// adoContains is contains: base is in target's history exactly when their
// common commit is base itself. target is a commit id or a branch name. The
// answer lists changes, which nothing here reads, so one is asked for.
func (f *forgeRepo) adoContains(ctx context.Context, target, base string) (string, error) {
	targetType := "branch"
	if len(target) == 40 && isHex(target) {
		targetType = "commit"
	}
	var diff struct {
		CommonCommit string `json:"commonCommit"`
	}
	status, err := f.adoGet(ctx, "/diffs/commits?$top=1&baseVersionType=commit&baseVersion="+url.QueryEscape(base)+
		"&targetVersionType="+targetType+"&targetVersion="+url.QueryEscape(target), &diff)
	switch {
	case errors.Is(err, errForgeTooLarge), status == http.StatusNotFound, status == http.StatusBadRequest:
		return "", nil
	case err != nil:
		return "", err
	case status != http.StatusOK:
		return "", fmt.Errorf("comparing %s with %s answered %d", target, base, status)
	case diff.CommonCommit != base:
		return "", nil
	}
	var commit struct {
		TreeID string `json:"treeId"`
	}
	status, err = f.adoGet(ctx, "/commits/"+url.PathEscape(base), &commit)
	switch {
	case err != nil:
		return "", err
	case status != http.StatusOK:
		return "", fmt.Errorf("reading commit %s answered %d", base, status)
	case !isHex(commit.TreeID):
		return "", fmt.Errorf("commit %s named no tree", base)
	}
	return commit.TreeID, nil
}

// adoTree lists one directory level. Azure DevOps spells a tree's mode 40000,
// as the tree object stores it.
func (f *forgeRepo) adoTree(ctx context.Context, tree string) ([]ghTreeEntry, error) {
	var v struct {
		ObjectID    string `json:"objectId"`
		TreeEntries []struct {
			RelativePath  string `json:"relativePath"`
			Mode          string `json:"mode"`
			GitObjectType string `json:"gitObjectType"`
			ObjectID      string `json:"objectId"`
		} `json:"treeEntries"`
	}
	status, err := f.adoGet(ctx, "/trees/"+url.PathEscape(tree), &v)
	switch {
	case err != nil:
		return nil, err
	case status != http.StatusOK:
		return nil, fmt.Errorf("listing tree %s answered %d", tree, status)
	case v.ObjectID != tree || v.TreeEntries == nil:
		return nil, fmt.Errorf("the forge did not list tree %s", tree)
	}
	entries := make([]ghTreeEntry, len(v.TreeEntries))
	for i, e := range v.TreeEntries {
		entries[i] = ghTreeEntry{Path: e.RelativePath, Mode: e.Mode, Type: strings.ToLower(e.GitObjectType), SHA: e.ObjectID}
	}
	return entries, nil
}

// fastForwards reports whether every ref cmds move goes forward from the tip it
// states: the history the pack carries for that ref's own new commit names the
// tip (named, from gitpack.Result.Reaches). A commit another ref sets, or one no
// ref sets, says nothing about this ref's move. A forward move through a commit
// the forge already holds reads as not, and so does any move once the walks were
// cut short (whole false), so either stays strict.
func fastForwards(cmds []gitpack.Command, named []map[string]bool, whole bool) bool {
	for i, c := range cmds {
		if strings.Trim(c.Old, "0") == "" || strings.Trim(c.New, "0") == "" {
			continue
		}
		if !whole || !named[i][c.Old] {
			return false
		}
	}
	return true
}
