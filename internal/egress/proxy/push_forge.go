// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// What a push leaves unchanged, read from the forge.
//
// A pack leaves out every object the forge already stores, so the inspector
// cannot tell a directory a push left alone from one it moved onto a denied
// path (push_rules.go). Object ids are content addresses, which is what closes
// that gap: an entry the pack does not carry is UNCHANGED exactly when the same
// path, in a commit the push builds on, has the same mode and object id — for a
// directory, that fixes everything beneath it. So for each such entry a deny
// pattern reaches, the broker reads that commit's trees from GitHub — trees
// only, one directory level per request, never blobs — and drops the entry when
// they agree. Anything else keeps the strict reading: the entry refuses the
// push, and the refusal says why it could not be dropped.
//
// WHICH COMMIT. A commit may name any object id as its parent, and on GitHub
// "the forge holds it" is not "this repository's history": objects pushed to
// any fork of a repository are readable, and pushable-upon, through the
// repository itself. Comparing against whatever parent the pack names would
// let a fork's commit vouch for its own content at a denied path. So a base
// counts only when the forge reports it inside the CURRENT history of a branch
// this push updates or of the default branch (contains) — the forge resolves
// those names, so nothing the pack asserts is taken on trust. A merge passes
// an entry that matches ANY counted parent: demanding all would refuse
// merging the default branch in after it changed a denied path, and closes
// nothing a single-parent commit on the same base could not already do.
//
// WHAT THE PACK RE-SENDS. git leaves out of a pack only what is reachable from
// the tips the forge advertises that the client also has. A clone taken before
// its branch moved on has none of them, so it re-sends its history — its
// shallow boundary commit, or everything — and that history's first commit is
// read whole: every file the repository already has arrives as though the push
// added it. So before any carried entry refuses a push, the commits the pack
// carries are put to the same question a base is (vouch), and the ones the
// forge already holds in the default branch's or an updated branch's history
// are taken out of the answer (gitpack.Result.Settle). A push built on one is
// then judged against it exactly as against a parent it did not re-send.
//
// THE PRICE. The broker dials api.github.com with the run's own credential
// for the lane — normally already minted by the push's discovery request, so
// the read is a cache hit — and only for a push the pack alone would refuse.
// At most maxForgeReads requests, all inside forgeReadWait, all while the push
// holds the sidecar's inspection slot. Any
// failure — a forge other than GitHub, a status other than 200, a truncated
// listing, the read budget or the wait running out — keeps the strict reading.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/gitpack"
)

const (
	// githubAPIHost is where GitHub serves the tree, compare and repository
	// reads. Both lanes' GitHub traffic is github.com, so this is the one API.
	githubAPIHost = "api.github.com"
	// maxForgeBody bounds one API response. A tree listing is a few hundred
	// bytes per entry, so this is a directory of roughly ten thousand entries;
	// one larger fails closed.
	maxForgeBody = 4 << 20

	// The reasons a refusal gives for entries the forge could not clear.
	whyChanged   = "a directory or file above is not what the same path held in the commit this push builds on"
	whyNoBase    = "this push builds on no commit in the history of the default branch or of a branch it updates, so nothing vouches for what it does not carry"
	whyNotGitHub = "this forge is not GitHub, so what the push does not carry cannot be compared with the commit it builds on"
	whyUnread    = "the broker could not compare what this push does not carry with the commit it builds on: "
	whyUnsettled = "the broker could not ask the forge which commits this push carries it already holds, so every one of them is read as new: "
)

// maxForgeReads and forgeReadWait bound what one push may cost the forge and
// the inspection slot. Vars so tests can shrink them.
var (
	maxForgeReads = 64
	forgeReadWait = 20 * time.Second
)

// errForgeTooLarge is an API response over maxForgeBody.
var errForgeTooLarge = fmt.Errorf("a forge response exceeded %d bytes", maxForgeBody)

// forgeRepo reads one GitHub repository's history for one push. Built per
// request; nil for a lane that cannot read GitHub (a git_pat host that is not
// github.com), which keeps the strict reading.
type forgeRepo struct {
	p *Proxy
	// repo is "owner/name", already validated by the lane.
	repo string
	// token is the lane's own credential lookup.
	token func(context.Context) (string, error)

	tok           string
	reads         int
	defaultBranch string
	// listings caches tree listings by tree id; a content address, so one
	// cache serves every base.
	listings map[string]map[string]ghTreeEntry
}

// ghTreeEntry is one entry of GitHub's git/trees listing.
type ghTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// patForge is the git_pat lane's reader: GitHub only, and only for a path
// that names exactly one repository, "/<owner>/<name>[.git]/git-receive-pack".
func (p *Proxy) patForge(host, rest string, g PATGrant) *forgeRepo {
	path, ok := strings.CutSuffix(rest, "/git-receive-pack")
	if host != githubHost || !ok {
		return nil
	}
	owner, name, ok := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(path, "/"), ".git"), "/")
	if !ok || !gitSegSafe(owner) || !gitSegSafe(name) {
		return nil
	}
	return &forgeRepo{p: p, repo: owner + "/" + name, token: func(ctx context.Context) (string, error) {
		tok, _, err := p.patToken(ctx, g)
		return tok, err
	}}
}

// unchanged returns the entries it could NOT show unchanged, and why.
func (f *forgeRepo) unchanged(ctx context.Context, entries []gitpack.Change, res gitpack.Result) ([]gitpack.Change, string) {
	if f == nil {
		return entries, whyNotGitHub
	}
	var roots []string
	for _, base := range res.Bases {
		root, err := f.vouch(ctx, base, res.Commands)
		if err != nil {
			return entries, f.unread(whyUnread, err)
		}
		if root != "" {
			roots = append(roots, root)
		}
	}
	if len(roots) == 0 {
		return entries, whyNoBase
	}
	var left []gitpack.Change
	for i, c := range entries {
		held, err := f.heldAtAny(ctx, roots, c)
		if err != nil {
			return append(left, entries[i:]...), f.unread(whyUnread, err)
		}
		if !held {
			left = append(left, c)
		}
	}
	return left, whyChanged
}

func (f *forgeRepo) unread(why string, err error) string {
	return why + f.p.redactTopology(string(maskDecisionBytes([]byte(err.Error()))))
}

// settle takes out of res the history the pack re-sends: every carried commit
// vouch counts, and everything beneath it. On a failure res comes back as it
// was — every carried commit read as new — with the reason.
func (f *forgeRepo) settle(ctx context.Context, res gitpack.Result) (gitpack.Result, string) {
	if f == nil {
		return res, ""
	}
	settled, err := res.Settle(func(c string) (bool, error) {
		root, err := f.vouch(ctx, c, res.Commands)
		return root != "", err
	})
	if err != nil {
		return res, f.unread(whyUnsettled, err)
	}
	return settled, ""
}

// vouch returns base's root tree when a branch this push updates, or the
// default branch, contains base; "" when none does.
func (f *forgeRepo) vouch(ctx context.Context, base string, cmds []gitpack.Command) (string, error) {
	for _, c := range cmds {
		branch, ok := strings.CutPrefix(c.Ref, "refs/heads/")
		if !ok || strings.Trim(c.Old, "0") == "" { // a create names no branch the forge has yet
			continue
		}
		if root, err := f.contains(ctx, branch, base); err != nil || root != "" {
			return root, err
		}
	}
	if f.defaultBranch == "" {
		var repo struct {
			DefaultBranch string `json:"default_branch"`
		}
		status, err := f.get(ctx, "/repos/"+f.repo, &repo)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK || repo.DefaultBranch == "" {
			return "", fmt.Errorf("reading the repository's default branch answered %d", status)
		}
		f.defaultBranch = repo.DefaultBranch
	}
	return f.contains(ctx, f.defaultBranch, base)
}

// contains asks the forge whether branch's current history includes base, and
// returns base's root tree if it does. base is in branch's history exactly
// when their merge base is base itself; such an answer lists no commits and no
// files, so it is small, and one too large to read is not one.
func (f *forgeRepo) contains(ctx context.Context, branch, base string) (string, error) {
	var cmp struct {
		MergeBase struct {
			SHA    string `json:"sha"`
			Commit struct {
				Tree struct {
					SHA string `json:"sha"`
				} `json:"tree"`
			} `json:"commit"`
		} `json:"merge_base_commit"`
	}
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	status, err := f.get(ctx, "/repos/"+f.repo+"/compare/"+strings.Join(segs, "/")+"..."+base, &cmp)
	switch {
	case errors.Is(err, errForgeTooLarge), status == http.StatusNotFound, status == http.StatusUnprocessableEntity:
		return "", nil
	case err != nil:
		return "", err
	case status != http.StatusOK:
		return "", fmt.Errorf("comparing %s with %s answered %d", branch, base, status)
	case cmp.MergeBase.SHA != base:
		return "", nil
	case !isHex(cmp.MergeBase.Commit.Tree.SHA):
		return "", fmt.Errorf("comparing %s with %s named no tree", branch, base)
	}
	return cmp.MergeBase.Commit.Tree.SHA, nil
}

// heldAtAny reports whether any of roots holds c's mode and object id at c's
// path. An uncarried directory is matched on its tree id alone: GitHub spells
// a tree's mode 040000 where the tree object stores 40000.
func (f *forgeRepo) heldAtAny(ctx context.Context, roots []string, c gitpack.Change) (bool, error) {
	for _, root := range roots {
		e, ok, err := f.entryAt(ctx, root, c.Path)
		if err != nil {
			return false, err
		}
		if !ok || e.SHA != c.OID {
			continue
		}
		if c.Mode == gitpack.ModeUncarried && e.Type == "tree" || c.Mode == e.Mode {
			return true, nil
		}
	}
	return false, nil
}

// entryAt walks root one directory level per listing down to path.
func (f *forgeRepo) entryAt(ctx context.Context, root, path string) (ghTreeEntry, bool, error) {
	e := ghTreeEntry{Type: "tree", SHA: root}
	if path == "" {
		return e, true, nil
	}
	for _, seg := range strings.Split(path, "/") {
		if e.Type != "tree" {
			return ghTreeEntry{}, false, nil
		}
		entries, err := f.listing(ctx, e.SHA)
		if err != nil {
			return ghTreeEntry{}, false, err
		}
		var ok bool
		if e, ok = entries[seg]; !ok {
			return ghTreeEntry{}, false, nil
		}
	}
	return e, true, nil
}

func (f *forgeRepo) listing(ctx context.Context, tree string) (map[string]ghTreeEntry, error) {
	if l, ok := f.listings[tree]; ok {
		return l, nil
	}
	var v struct {
		Tree      []ghTreeEntry `json:"tree"`
		Truncated bool          `json:"truncated"`
	}
	status, err := f.get(ctx, "/repos/"+f.repo+"/git/trees/"+tree, &v)
	switch {
	case err != nil:
		return nil, err
	case status != http.StatusOK:
		return nil, fmt.Errorf("listing tree %s answered %d", tree, status)
	case v.Truncated:
		return nil, fmt.Errorf("the forge truncated its listing of tree %s", tree)
	}
	l := make(map[string]ghTreeEntry, len(v.Tree))
	for _, e := range v.Tree {
		if !isHex(e.SHA) {
			return nil, fmt.Errorf("tree %s lists %q with no object id", tree, e.Path)
		}
		l[e.Path] = e
	}
	if f.listings == nil {
		f.listings = map[string]map[string]ghTreeEntry{}
	}
	f.listings[tree] = l
	return l, nil
}

// get reads one GitHub API path into v and reports the status. Only a 200 is
// decoded; every request counts against maxForgeReads.
func (f *forgeRepo) get(ctx context.Context, path string, v any) (int, error) {
	if f.reads >= maxForgeReads {
		return 0, fmt.Errorf("the comparison needs more than %d reads of the forge", maxForgeReads)
	}
	f.reads++
	if f.tok == "" {
		tok, err := f.token(ctx)
		if err != nil {
			return 0, fmt.Errorf("the run's credential: %w", err)
		}
		f.tok = tok
	}
	target, _, err := f.p.egressTarget(githubAPIHost, 443)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, vettedIPKey{}, target),
		http.MethodGet, "https://"+githubAPIHost+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+f.tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := f.p.transport.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxForgeBody+1))
	if err != nil {
		return 0, err
	}
	if len(body) > maxForgeBody {
		return http.StatusOK, errForgeTooLarge
	}
	return http.StatusOK, json.Unmarshal(body, v)
}

// isHex reports a non-empty lowercase hex string: an object id as the forge
// spells one, safe to put in a request path.
func isHex(s string) bool {
	return s != "" && strings.Trim(s, "0123456789abcdef") == ""
}
