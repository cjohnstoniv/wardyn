// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

// Which Azure DevOps REST requests write git repository CONTENT, for a run's
// push_rules (internal/egress/proxy's ADO REST gate).
//
// The capability catalogue above answers "may this run do this at all"; push
// rules ask "what does this write put in the repository", and the REST API is
// a second door to that question beside git itself. Git Pushes - Create
// (POST …/_apis/git/repositories/{repo}/pushes) carries its files inline,
// path by path, so its paths can be read (ParsePush). Every other route that
// puts content on a branch — an import, a server-side commit, merge,
// cherry-pick or revert, a fork sync, a ref or tag pointed at a commit the
// request does not show, a pull-request completion, a wiki page (a wiki is a
// git repository, and a code wiki is a branch of one), a TFVC check-in — names
// no path the rules could judge, so it is reported ContentOpaque and the gate
// refuses it while the run has push rules. That mirrors the git door, which
// refuses a ref update that carries no pack.
//
// Routes are read with the classifier's own parser (parseRoute), so the two
// cannot disagree about which resource a path names.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// ContentWrite is how a request writes repository content.
type ContentWrite int

const (
	// NoContentWrite: a read, or a write that puts nothing on a branch.
	NoContentWrite ContentWrite = iota
	// ContentPush: Git Pushes - Create; ParsePush reads its paths.
	ContentPush
	// ContentOpaque: content reaches a branch by a route that names no path.
	ContentOpaque
)

// ContentTarget is a content-writing request's classification and, for one
// under _apis/git/repositories/{repo}, the repository: RepoPath is
// "[<project>/]_git/<repo>" relative to the organisation, lower-cased.
type ContentTarget struct {
	Write    ContentWrite
	RepoPath string
}

// gitOpaqueContentResources are the repository sub-resources that write
// content without naming its paths.
var gitOpaqueContentResources = []string{
	"importrequests", "items", "commits", "merges", "cherrypicks", "reverts",
	"suggestions", "forksyncrequests", "annotatedtags",
}

// ClassifyContent reports how req writes repository content. A route whose
// answer depends on the body (a ref update, a pull-request update) answers
// ErrNeedsBody for a BodyWithheld request, exactly as Classify does; one it
// cannot read is an error, which the caller refuses.
func ClassifyContent(req Request) (ContentTarget, error) {
	method, err := effectiveMethod(req.Method, req.Header)
	if err != nil {
		return ContentTarget{}, err
	}
	r, err := parseRoute(req.Host, req.Path, req.Org)
	if err != nil {
		return ContentTarget{}, err
	}
	if method == http.MethodOptions || slices.Contains(readMethods, method) || readWrite(r) {
		return ContentTarget{}, nil
	}
	switch r.area {
	case "wiki", "tfvc":
		return ContentTarget{Write: ContentOpaque}, nil
	case "git":
	default:
		return ContentTarget{}, nil
	}
	if r.res == "pullrequests" {
		return pullRequestContent(method, req, ContentTarget{}, r.at(4))
	}
	if r.res != "repositories" || r.at(3) == "" {
		return ContentTarget{}, nil
	}
	t := ContentTarget{RepoPath: strings.Join(append(slices.Clone(r.segs[:r.apis]), "_git", r.at(3)), "/")}
	// method is the EFFECTIVE method (X-HTTP-Method-Override applied), and
	// every write to these resources is content whatever verb it arrives as:
	// only a POST to pushes has a body ParsePush knows how to read, so any
	// other verb there is refused as opaque rather than waved through.
	switch res := r.at(4); {
	case res == "pushes" && method == http.MethodPost:
		t.Write = ContentPush
	case res == "pushes" || slices.Contains(gitOpaqueContentResources, res):
		t.Write = ContentOpaque
	case res == "refs" && method == http.MethodPost:
		return refContent(req, t)
	case res == "pullrequests":
		return pullRequestContent(method, req, t, r.at(6))
	}
	return t, nil
}

// refContent: Update Refs is content when any update points a ref at a
// commit — the request shows no content, only an object id the service
// already holds. An update that only deletes refs is not.
func refContent(req Request, t ContentTarget) (ContentTarget, error) {
	body, err := peekBody(req)
	if err != nil {
		return t, err
	}
	var updates []struct {
		NewObjectID string `json:"newObjectId"`
	}
	if err := decodeUnique(body, &updates); err != nil {
		return t, err
	}
	for _, u := range updates {
		if strings.Trim(u.NewObjectID, "0") != "" {
			t.Write = ContentOpaque
		}
	}
	return t, nil
}

// pullRequestContent: completing a pull request merges its source into the
// target branch, content the request does not show — whether it completes now
// (status "completed") or sets auto-complete to do so later, and whether that
// is asked of an existing pull request or of one being created. Only the pull
// request object itself carries those fields; sub is the segment after its id
// (threads, reviewers, statuses, …), whose writes carry no content.
func pullRequestContent(method string, req Request, t ContentTarget, sub string) (ContentTarget, error) {
	if method == http.MethodDelete || sub != "" {
		return t, nil
	}
	body, err := peekBody(req)
	if err != nil {
		return t, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return t, nil
	}
	var pr struct {
		Status            string `json:"status"`
		AutoCompleteSetBy any    `json:"autoCompleteSetBy"`
	}
	if err := decodeUnique(body, &pr); err != nil {
		return t, err
	}
	if strings.EqualFold(pr.Status, "completed") || pr.AutoCompleteSetBy != nil {
		t.Write = ContentOpaque
	}
	return t, nil
}

// Push is what a Git Pushes - Create body writes: the refs it moves, every
// path its changes name (a rename names both), and a digest of the body — the
// request names no commit until the service makes one, so the digest is what
// identifies its content.
type Push struct {
	Refs   []string
	Paths  []string
	Digest string
}

// ParsePush reads a Git Pushes - Create body the classifier has already let
// through peekBody. Anything it cannot read whole is an error: a repeated
// key, a change with no path, a path git would not store, a push that moves
// no ref.
func ParsePush(req Request) (Push, error) {
	body, err := peekBody(req)
	if err != nil {
		return Push{}, err
	}
	refs, err := refNames(body)
	if err != nil {
		return Push{}, err
	}
	var push struct {
		Commits []struct {
			Changes []struct {
				Item *struct {
					Path string `json:"path"`
				} `json:"item"`
				SourceServerItem string `json:"sourceServerItem"`
			} `json:"changes"`
		} `json:"commits"`
	}
	if err := decodeUnique(body, &push); err != nil {
		return Push{}, err
	}
	out := Push{Refs: refs}
	for _, c := range push.Commits {
		for _, ch := range c.Changes {
			if ch.Item == nil || ch.Item.Path == "" {
				return Push{}, fmt.Errorf("adoscope: a push change names no item path")
			}
			for _, raw := range []string{ch.Item.Path, ch.SourceServerItem} {
				if raw == "" {
					continue // no rename source
				}
				p, err := pushPath(raw)
				if err != nil {
					return Push{}, err
				}
				out.Paths = append(out.Paths, p)
			}
		}
	}
	sum := sha256.Sum256(body)
	out.Digest = hex.EncodeToString(sum[:])
	return out, nil
}

// pushPath is a change's path as git stores it: the leading "/" dropped, and
// refused when it could name something other than what it reads as — an
// empty, "." or ".." segment, a backslash (a separator to the service), or a
// control byte — or when it names the repository root, which no rule can be
// matched against.
func pushPath(raw string) (string, error) {
	p := strings.TrimPrefix(raw, "/")
	if strings.TrimSuffix(p, "/") == "" {
		return "", fmt.Errorf("adoscope: push path %q names the repository root", raw)
	}
	if strings.ContainsFunc(p, func(c rune) bool { return c == '\\' || c < 0x20 || c == 0x7f }) {
		return "", fmt.Errorf("adoscope: push path %q carries a backslash or a control character", raw)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("adoscope: push path %q has an empty, \".\" or \"..\" segment", raw)
		}
	}
	return strings.TrimSuffix(p, "/"), nil
}
