// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The Azure DevOps arm of a repository draft's read, modelled on the branch list's (adoRepoBranches): the same
// target, the same credential (the person's own sign-in or token), the same API version and no redirect followed.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoFileJSONFactor is how far a file's JSON encoding may outgrow the file: a control byte becomes six.
const adoFileJSONFactor = 6

// adoGetFunc is one GET of the Azure DevOps API that decodes a 200 into out and returns "" or the reason it could
// not. adoBranchGet is the branch list's; adoFileGet reads a file under a byte cap.
type adoGetFunc func(ctx context.Context, target, auth string, out any) string

// adoRepoFiles is the Azure DevOps arm: the person's own token (own_pat, Services or Server) or their own
// sign-in (bearer). A minted_pat row's sign-in can only create tokens, so it reads nothing.
func (s *Server) adoRepoFiles(ctx context.Context, sc types.SiteConfig, a adoEntraRun, repo string) (assistantRepoRead, *assistantRefusal) {
	out := assistantRepoRead{credential: branchCredADOSignIn}
	if a.tokenMode == types.ADOTokenModeOwnPAT {
		out.credential = branchCredADOOwnToken
	}
	if a.tokenMode == types.ADOTokenModeMintedPAT {
		return assistantRepoRead{}, assistantRepoNoCredential()
	}
	base, project, name, ok := adoBranchTarget(sc, a, repo)
	if !ok {
		return out, assistantRepoHostUnsupported()
	}
	auth, reason, err := s.adoBranchAuth(ctx, a)
	if err != nil {
		return out, assistantRepoUnreachable()
	}
	if reason != "" {
		return out, adoReadRefusal(reason)
	}
	repoURL := base + "/" + url.PathEscape(project) + "/_apis/git/repositories/" + url.PathEscape(name)
	branch, files, reason := adoReadRootFiles(ctx, adoBranchGet, adoFileGet, repoURL, auth, assistant.BuildFileNames(), assistant.MaxBuildFileBytes)
	if reason != "" {
		return out, adoReadRefusal(reason)
	}
	out.branch, out.files = branch, files
	return out, nil
}

// adoReadRefusal is the refusal for a reason the branch list's arm would have given.
func adoReadRefusal(reason string) *assistantRefusal {
	switch reason {
	case branchReasonNoCredential:
		return assistantRepoNoCredential()
	case branchReasonSignInNeeded:
		return assistantRepoSignInNeeded()
	case branchReasonRefused:
		return assistantRepoReadRefused()
	case branchReasonNotSupported:
		return assistantRepoHostUnsupported()
	}
	return assistantRepoUnreachable()
}

// adoReadRootFiles reads the repository's default branch, lists its root, and reads each listed name that is a
// file at the root (a blob, never a folder), in the order names gives them. A file that is not under maxBytes is
// named with TooLarge and no text. The reason is "" or the one the branch list would give.
func adoReadRootFiles(ctx context.Context, get, getFile adoGetFunc, repoURL, auth string, names []string, maxBytes int) (branch string, files []broker.File, reason string) {
	var meta struct {
		DefaultBranch string `json:"defaultBranch"`
	}
	if reason := get(ctx, repoURL+"?api-version="+adoBranchAPIVersion, auth, &meta); reason != "" {
		return "", nil, reason
	}
	branch = strings.TrimPrefix(meta.DefaultBranch, "refs/heads/")
	if branch == "" {
		return "", nil, ""
	}
	version := url.Values{"versionDescriptor.version": {branch}, "versionDescriptor.versionType": {"branch"}, "api-version": {adoBranchAPIVersion}}
	list := url.Values{"scopePath": {"/"}, "recursionLevel": {"OneLevel"}}
	for k, v := range version {
		list[k] = v
	}
	var root struct {
		Value []struct {
			Path          string `json:"path"`
			GitObjectType string `json:"gitObjectType"`
			IsFolder      bool   `json:"isFolder"`
		} `json:"value"`
	}
	if reason := get(ctx, repoURL+"/items?"+list.Encode(), auth, &root); reason != "" {
		return "", nil, reason
	}
	var blobs []string
	for _, it := range root.Value {
		if n, ok := strings.CutPrefix(it.Path, "/"); ok && !it.IsFolder && it.GitObjectType == "blob" && slices.Contains(names, n) {
			blobs = append(blobs, n)
		}
	}
	for _, name := range names {
		if !slices.Contains(blobs, name) {
			continue
		}
		q := url.Values{"path": {"/" + name}, "includeContent": {"true"}}
		for k, v := range version {
			q[k] = v
		}
		var item struct {
			Content string `json:"content"`
		}
		reason := getFile(ctx, repoURL+"/items?"+q.Encode(), auth, &item)
		f := broker.File{Name: name, Branch: branch, Size: len(item.Content)}
		switch {
		case reason == branchReasonFileTooLarge, len(item.Content) > maxBytes:
			f.TooLarge, f.Size = true, max(f.Size, maxBytes+1)
		case reason != "":
			return "", nil, reason
		default:
			f.Text = item.Content
		}
		files = append(files, f)
	}
	return branch, files, ""
}

// branchReasonFileTooLarge marks a file whose answer outgrew the cap: the file is skipped, not a failed read.
const branchReasonFileTooLarge = "file_too_large"

// adoFileGet is adoBranchGet for one file: the answer is read up to a cap above the largest file's encoding, and
// one that outgrows it is reported as too large, not as unreachable.
func adoFileGet(ctx context.Context, target, auth string, out any) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return branchReasonUnreachable
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{
		Transport:     adoOwnPATTransport,
		Timeout:       adoEntraRedeemTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return branchReasonUnreachable
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case adoPATTransientStatus(resp.StatusCode):
		return branchReasonUnreachable
	case resp.StatusCode != http.StatusOK:
		return branchReasonRefused
	}
	limit := int64(assistant.MaxBuildFileBytes*adoFileJSONFactor + 4096)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	switch {
	case err != nil:
		return branchReasonUnreachable
	case int64(len(raw)) > limit:
		return branchReasonFileTooLarge
	case json.Unmarshal(raw, out) != nil:
		return branchReasonUnreachable
	}
	return ""
}
