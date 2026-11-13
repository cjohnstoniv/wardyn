// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// What a push leaves unchanged, read from the forge.
//
// A pack leaves out every object the forge already stores, so the inspector
// can't tell a directory a push left alone from one it moved onto a denied
// path (push_rules.go). Object ids are content addresses, which closes that
// gap: an entry the pack doesn't carry is UNCHANGED exactly when the same
// path, in a commit the push builds on, has the same mode and object id —
// for a directory, that fixes everything beneath it. So for each such entry
// a deny pattern reaches, the broker reads that commit's trees from GitHub
// — trees only, one directory level per request, never blobs — and drops
// the entry when they agree. Anything else keeps the strict reading.
//
// WHICH COMMIT. A commit may name any object id as its parent, and on
// GitHub "the forge holds it" is not "this repository's history": objects
// pushed to any fork are readable, and pushable-upon, through the
// repository itself. Comparing against whatever parent the pack names would
// let a fork's commit vouch for its own content at a denied path. So a base
// counts only when the forge reports it inside the CURRENT history of a
// branch this push updates or of the default branch (contains). An updated
// branch counts only at the tip the forge reports, and only when that tip
// is the old value the push states (updatedTip): neither the branch name
// nor the old value the push asserts is taken on trust. Nor does every
// commit the pack carries count: one outside the history of every ref the
// push sets builds on nothing the push builds on, so a base counts only when
// the history the pack carries for some ref's new commit names it (Reaches). A
// merge passes an entry matching ANY counted parent: demanding all would
// refuse merging the default branch into a branch the push creates after it
// changed a denied path. A counted parent can be older than the tip of a
// branch the push updates, so for deny_paths and require_review_paths that
// branch is also compared with its own tip (push_forge_tip.go).
//
// WHAT THE PACK RE-SENDS. git leaves out of a pack only what is reachable
// from the tips the forge advertises that the client also has. A clone
// taken before its branch moved on has none of them, so it re-sends its
// history — its shallow boundary commit, or everything — and that history's
// first commit is read whole: every file the repository already has arrives
// as though the push added it. So before any carried entry refuses a push,
// the commits the pack carries face the same question a base does (vouch),
// and the ones the forge already holds in the default or an updated
// branch's history are taken out of the answer (gitpack.Result.Settle). A
// push built on one is then judged exactly as against a parent it didn't
// re-send.
//
// THE PRICE. The broker dials api.github.com with the run's own credential
// for the lane — normally already minted by the push's discovery request,
// so the read is a cache hit — and only for a push the pack alone would
// refuse. At most maxForgeReads requests, all inside forgeReadWait, all
// while the push holds the sidecar's inspection slot. Any failure — a forge
// with no reader, a status other than 200, a truncated listing, the read
// budget or wait running out — keeps the strict reading. Azure DevOps answers
// the same questions over its own API (push_forge_ado.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
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
	whyChanged = "a directory or file above is not what the same path held in the commit this push builds on"
	// whyNotExecutable is whyChanged for deny_new_executables, whose forge test is the mode alone.
	whyNotExecutable = "was not an executable file at the same path in any commit this push builds on"
	whyNoBase        = "this push builds on no commit in the history of the default branch or of a branch it updates, so nothing vouches for what it does not carry"
	whyNoForgeReader = "the broker cannot read this forge's history, so what the push does not carry cannot be compared with the commit it builds on"
	whyUnread        = "the broker could not compare what this push does not carry with the commit it builds on: "
	whyUnsettled     = "the broker could not ask the forge which commits this push carries it already holds, so every one of them is read as new: "

	// The reasons a push that carries no object is refused (forgeRepo.held).
	whyHeldNoForgeReader = "the broker cannot read this forge's history, so it cannot ask whether the forge already holds these commits"
	whyHeldUnread        = "the broker could not ask the forge whether it already holds these commits: "
	whyNotHeld           = "the forge holds no such commit in the history of the default branch or of a branch this push updates, for: "
)

// maxForgeReads and forgeReadWait bound what one push may cost the forge and
// the inspection slot. Vars so tests can shrink them.
var (
	maxForgeReads = 64
	forgeReadWait = 20 * time.Second
)

// errForgeTooLarge is an API response over maxForgeBody.
var errForgeTooLarge = fmt.Errorf("a forge response exceeded %d bytes", maxForgeBody)

// forgeRepo reads one repository's history for one push: GitHub's, or Azure
// DevOps' when ado is set. Built per request; nil for a lane with no reader (a
// git_pat host that is neither), which keeps the strict reading.
type forgeRepo struct {
	p   *Proxy
	ado *adoForge
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
	// vouched and settled memoize the history questions, which the review
	// rules ask again after the deny rules: one push, one set of answers.
	vouched map[string]string
	settled *gitpack.Result
	// tips caches the forge's current tip of each branch a command names, read
	// once per push ("" when the forge holds no such branch).
	tips map[string]string
}

// ghTreeEntry is one entry of GitHub's git/trees listing.
type ghTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

// patForge is the git_pat lane's reader: on GitHub, only for a path
// that names exactly one repository, "/<owner>/<name>[.git]/git-receive-pack".
func (p *Proxy) patForge(host, rest string, g PATGrant) *forgeRepo {
	if adoHostedHost(host) {
		return p.adoPATForge(host, rest, g)
	}
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

// forgeClear is what clears one entry against the root trees of the counted
// bases: heldAtAny for a rule about paths and sizes, executableAtAny for
// deny_new_executables.
type forgeClear func(f *forgeRepo, ctx context.Context, roots []string, c gitpack.Change) (bool, error)

// packReaches is gitpack.Result.Reaches. A var so a test can cut a walk short.
var packReaches = gitpack.Result.Reaches

// unchanged returns the entries clear could NOT clear, and why.
func (f *forgeRepo) unchanged(ctx context.Context, entries []gitpack.Change, res gitpack.Result, clear forgeClear) ([]gitpack.Change, string) {
	if f == nil {
		return entries, whyNoForgeReader
	}
	named, whole := packReaches(res)
	if f.ado != nil && !fastForwards(res.Commands, named, whole) {
		if !whole {
			return entries, whyCutShort // a walk cut short shows no move either way
		}
		return entries, whyForced
	}
	var roots []string
	for _, base := range res.Bases {
		// A parent named only by a commit outside every pushed ref's history is nothing this push builds on.
		if !slices.ContainsFunc(named, func(n map[string]bool) bool { return n[base] }) {
			continue
		}
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
		held, err := clear(f, ctx, roots, c)
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
	if f.settled != nil {
		return *f.settled, ""
	}
	settled, err := res.Settle(func(c string) (bool, error) {
		root, err := f.vouch(ctx, c, res.Commands)
		return root != "", err
	})
	if err != nil {
		return res, f.unread(whyUnsettled, err)
	}
	f.settled = &settled
	return settled, ""
}

// held answers, for a push that carries no object, whether the forge already
// holds every commit cmds set a ref to: "" when vouch counts each one, or why
// not. A forge it cannot read (nil) cannot answer, so the push is refused.
func (f *forgeRepo) held(ctx context.Context, cmds []gitpack.Command) string {
	if f == nil {
		return whyHeldNoForgeReader
	}
	var unheld []string
	for _, c := range cmds {
		if strings.Trim(c.New, "0") == "" {
			continue
		}
		root, err := f.vouch(ctx, c.New, cmds)
		if err != nil {
			return f.unread(whyHeldUnread, err)
		}
		if root == "" {
			unheld = append(unheld, c.Ref)
		}
		if f.ado != nil && strings.Trim(c.Old, "0") != "" && root != "" {
			forward, err := f.adoContains(ctx, c.New, c.Old)
			if err != nil {
				return f.unread(whyHeldUnread, err)
			}
			if forward == "" {
				return whyHeldForced + c.Ref
			}
		}
	}
	if len(unheld) > 0 {
		return whyNotHeld + strings.Join(unheld, ", ")
	}
	return ""
}

// vouch returns base's root tree when a branch this push updates (at the tip
// the forge confirms for it, updatedTip), or the default branch, contains
// base; "" when none does.
func (f *forgeRepo) vouch(ctx context.Context, base string, cmds []gitpack.Command) (string, error) {
	if root, ok := f.vouched[base]; ok {
		return root, nil
	}
	root, err := f.vouchOnce(ctx, base, cmds)
	if err == nil {
		if f.vouched == nil {
			f.vouched = map[string]string{}
		}
		f.vouched[base] = root
	}
	return root, err
}

func (f *forgeRepo) vouchOnce(ctx context.Context, base string, cmds []gitpack.Command) (string, error) {
	for _, c := range cmds {
		tip, err := f.updatedTip(ctx, c)
		if err != nil {
			return "", err
		}
		if tip == "" {
			continue
		}
		if root, err := f.contains(ctx, tip, base); err != nil || root != "" {
			return root, err
		}
	}
	if f.ado != nil && f.defaultBranch == "" {
		branch, err := f.adoDefaultBranch(ctx)
		if err != nil || branch == "" {
			return "", err
		}
		f.defaultBranch = branch
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

// updatedTip returns the commit a command's branch stands at when the forge
// confirms it: the branch exists, its name is well formed, and its current tip
// is exactly the old value the command states. Only then is that branch's
// history one the forge holds for this repository; the ref name and old value
// are the push's own words, so nothing else about them is taken on trust. ""
// for a create, a command that moves nothing, a malformed name, or a tip the
// forge does not confirm. The history is read at the confirmed commit, not by
// name, so a branch that moves meanwhile changes nothing.
func (f *forgeRepo) updatedTip(ctx context.Context, c gitpack.Command) (string, error) {
	branch, ok := strings.CutPrefix(c.Ref, "refs/heads/")
	if !ok || !isHex(c.Old) || strings.Trim(c.Old, "0") == "" || c.New == c.Old || adoscope.CheckRefName(c.Ref) != nil {
		return "", nil
	}
	tip, seen := f.tips[c.Ref]
	if !seen && f.ado != nil {
		var err error
		if tip, err = f.adoBranchTip(ctx, c.Ref); err != nil {
			return "", err
		}
		if f.tips == nil {
			f.tips = map[string]string{}
		}
		f.tips[c.Ref] = tip
	} else if !seen {
		var ref struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		segs := strings.Split(branch, "/")
		for i, s := range segs {
			segs[i] = url.PathEscape(s)
		}
		status, err := f.get(ctx, "/repos/"+f.repo+"/git/ref/heads/"+strings.Join(segs, "/"), &ref)
		switch {
		case status == http.StatusNotFound:
		case err != nil:
			return "", err
		case status != http.StatusOK:
			return "", fmt.Errorf("reading branch %s answered %d", branch, status)
		case !isHex(ref.Object.SHA):
			return "", fmt.Errorf("reading branch %s named no commit", branch)
		default:
			tip = ref.Object.SHA
		}
		if f.tips == nil {
			f.tips = map[string]string{}
		}
		f.tips[c.Ref] = tip
	}
	if tip != c.Old {
		return "", nil
	}
	return tip, nil
}

// contains asks the forge whether branch's current history includes base, and
// returns base's root tree if it does. base is in branch's history exactly
// when their merge base is base itself; such an answer lists no commits and no
// files, so it is small, and one too large to read is not one.
func (f *forgeRepo) contains(ctx context.Context, branch, base string) (string, error) {
	if f.ado != nil {
		return f.adoContains(ctx, branch, base)
	}
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

// executableAtAny reports whether c is no new executable against any counted
// base: a directory the pack doesn't carry exactly when it is unchanged there,
// a file when the same path is an executable file there.
func (f *forgeRepo) executableAtAny(ctx context.Context, roots []string, c gitpack.Change) (bool, error) {
	if c.Mode == gitpack.ModeUncarried {
		return f.heldAtAny(ctx, roots, c)
	}
	for _, root := range roots {
		e, ok, err := f.entryAt(ctx, root, c.Path)
		if err != nil {
			return false, err
		}
		if ok && gitpack.IsExecutable(e.Mode) {
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
	entries, err := f.treeEntries(ctx, tree)
	if err != nil {
		return nil, err
	}
	l := make(map[string]ghTreeEntry, len(entries))
	for _, e := range entries {
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

func (f *forgeRepo) treeEntries(ctx context.Context, tree string) ([]ghTreeEntry, error) {
	if f.ado != nil {
		return f.adoTree(ctx, tree)
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
	return v.Tree, nil
}

// get reads one GitHub API path into v and reports the status.
func (f *forgeRepo) get(ctx context.Context, path string, v any) (int, error) {
	status, _, err := f.read(ctx, githubAPIHost, path, func(ctx context.Context, req *http.Request) error {
		if f.tok == "" {
			tok, err := f.token(ctx)
			if err != nil {
				return fmt.Errorf("the run's credential: %w", err)
			}
			f.tok = tok
		}
		req.Header.Set("Authorization", "Bearer "+f.tok)
		req.Header.Set("Accept", "application/vnd.github+json")
		return nil
	}, v)
	return status, err
}

// apiHost is the host this reader's requests go to, for the audit row.
func (f *forgeRepo) apiHost() string {
	if f.ado != nil {
		return f.ado.host
	}
	return githubAPIHost
}

// read makes one authorized GET of the forge and decodes a 200 into v; every
// request counts against maxForgeReads. The header is never nil.
func (f *forgeRepo) read(ctx context.Context, host, path string, authorize func(context.Context, *http.Request) error, v any) (int, http.Header, error) {
	none := http.Header{}
	if f.reads >= maxForgeReads {
		return 0, none, fmt.Errorf("the comparison needs more than %d reads of the forge", maxForgeReads)
	}
	f.reads++
	target, _, err := f.p.egressTarget(host, 443)
	if err != nil {
		return 0, none, err
	}
	req, err := http.NewRequestWithContext(context.WithValue(withProxyOwnSend(ctx), vettedIPKey{}, target),
		http.MethodGet, "https://"+upstreamURLHost(host, 443, 443)+path, nil)
	if err != nil {
		return 0, none, err
	}
	if err := authorize(ctx, req); err != nil {
		return 0, none, err
	}
	// roundTripUpstream, not the transport: the forge host may speak HTTP/2
	// unasked, and a bodyless GET is always resendable over the fallback.
	resp, err := f.p.roundTripUpstream(req)
	if err != nil {
		return 0, none, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, resp.Header, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxForgeBody+1))
	if err != nil {
		return 0, none, err
	}
	if len(body) > maxForgeBody {
		return http.StatusOK, resp.Header, errForgeTooLarge
	}
	return http.StatusOK, resp.Header, json.Unmarshal(body, v)
}

// isHex reports a non-empty lowercase hex string: an object id as the forge
// spells one, safe to put in a request path.
func isHex(s string) bool {
	return s != "" && strings.Trim(s, "0123456789abcdef") == ""
}
