// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The closed operation tables of the git_pat forge API door (pat_api.go). A
// request is admitted only when its method and path equal a row of its forge's
// table, under a repository path the grant names. Nothing is matched by prefix,
// so a path the table does not list is refused, never guessed at.
//
// What a table never lists, by design: merge and auto-merge, writes to
// repository files and commits (they bypass the receive-pack content and branch
// checks), GraphQL and search, project metadata (GitLab returns runner tokens to
// a maintainer), variables, hooks, keys, tokens, members and exports, and any
// endpoint that does not sit under one repository's own path.

// patAPIKind is what a row does, which is what access: read judges.
type patAPIKind int

const (
	patAPIRead    patAPIKind = iota // GET of the repository's own data
	patAPICreate                    // open a merge or pull request
	patAPIComment                   // comment on a merge or pull request, or on an issue
)

type patAPIRow struct {
	method string
	tail   string // "/"-joined pattern under the repository path; see patAPITokenMatches
	kind   patAPIKind
}

// patAPISeg is one decoded path segment. slash is true when the raw segment
// carried an encoded "/" (%2F), which only a repository path or a file path may.
type patAPISeg struct {
	s     string
	slash bool
}

// patAPIForge is one forge's table and the three places it differs from another.
type patAPIForge struct {
	// repo splits the leading segments into the repository key and the tail
	// beneath it. why is "" on success.
	repo func(segs []patAPISeg) (key string, tail []patAPISeg, why string)
	rows []patAPIRow
	// header is the credential header the forge reads.
	header func(token string) (name, value string)
	// check applies the forge's own body rules to an admitted row.
	check func(row patAPIRow, tail []patAPISeg, repo string, f *patAPIFields) string
}

func patAPIReads(tails ...string) []patAPIRow {
	rows := make([]patAPIRow, len(tails))
	for i, t := range tails {
		rows[i] = patAPIRow{http.MethodGet, t, patAPIRead}
	}
	return rows
}

func patAPIWrites(kind patAPIKind, tails ...string) []patAPIRow {
	rows := make([]patAPIRow, len(tails))
	for i, t := range tails {
		rows[i] = patAPIRow{http.MethodPost, t, kind}
	}
	return rows
}

func patAPIRowsOf(groups ...[]patAPIRow) []patAPIRow { return slices.Concat(groups...) }

// patAPIForges is keyed by the grant's forge. generic has no table: a forge the
// proxy knows no API shape for is refused whole.
var patAPIForges = map[string]*patAPIForge{
	types.PATForgeGitLab: {
		repo:   gitlabRepo,
		header: func(tok string) (string, string) { return "Private-Token", tok },
		check:  gitlabCheck,
		rows: patAPIRowsOf(
			patAPIReads(
				"merge_requests", "merge_requests/{n}", "merge_requests/{n}/changes", "merge_requests/{n}/diffs",
				"merge_requests/{n}/commits", "merge_requests/{n}/notes", "merge_requests/{n}/notes/{n}",
				"issues", "issues/{n}", "issues/{n}/notes", "issues/{n}/notes/{n}",
				"repository/branches", "repository/branches/*", "repository/tags", "repository/tags/*",
				"repository/commits", "repository/commits/*", "repository/commits/*/diff", "repository/compare",
				"repository/tree", "repository/files/{f}", "repository/files/{f}/raw",
				"repository/blobs/*", "repository/blobs/*/raw", "repository/{archive}",
				"pipelines", "pipelines/{n}", "pipelines/{n}/jobs", "jobs", "jobs/{n}",
			),
			patAPIWrites(patAPICreate, "merge_requests"),
			patAPIWrites(patAPIComment, "merge_requests/{n}/notes", "issues/{n}/notes"),
		),
	},
	types.PATForgeGitea: {
		repo:   giteaRepo,
		header: func(tok string) (string, string) { return "Authorization", "token " + tok },
		check:  giteaCheck,
		rows: patAPIRowsOf(
			patAPIReads(
				"pulls", "pulls/{n}", "pulls/{n}.diff", "pulls/{n}.patch", "pulls/{n}/files", "pulls/{n}/commits",
				"issues", "issues/{n}", "issues/{n}/comments", "issues/comments", "issues/comments/{n}",
				"branches", "branches/**", "tags", "tags/*", "commits", "commits/*/status", "commits/*/statuses",
				"git/commits/*", "git/trees/*", "git/blobs/*", "compare/**", "statuses/*",
				"actions/runs", "actions/runs/{n}", "actions/runs/{n}/jobs", "actions/jobs",
				"contents", "contents/**", "raw/**", "media/**", "archive/**",
			),
			patAPIWrites(patAPICreate, "pulls"),
			patAPIWrites(patAPIComment, "issues/{n}/comments"),
		),
	},
	types.PATForgeBitbucketServer: {
		repo:   bitbucketRepo,
		header: func(tok string) (string, string) { return "Authorization", "Bearer " + tok },
		check:  bitbucketCheck,
		rows: patAPIRowsOf(
			patAPIReads(
				"pull-requests", "pull-requests/{n}", "pull-requests/{n}/changes", "pull-requests/{n}/diff",
				"pull-requests/{n}/commits", "pull-requests/{n}/activities", "pull-requests/{n}/comments",
				"pull-requests/{n}/comments/{n}",
				"branches", "tags", "commits", "commits/*", "commits/*/changes",
				"compare/changes", "compare/commits", "browse", "browse/**", "raw/**", "files", "files/**", "archive",
			),
			patAPIWrites(patAPICreate, "pull-requests"),
			patAPIWrites(patAPIComment, "pull-requests/{n}/comments"),
		),
	},
}

// patAPISplit splits the path as it arrived on the wire (still percent-encoded)
// into decoded segments, refusing every spelling the tables do not compare: an
// empty segment, ".", "..", a backslash, a semicolon, a control character, and a
// decoded "%" (a double encoding). The forward re-sends the same raw path, so
// the string compared here is the string the forge routes.
func patAPISplit(raw string) ([]patAPISeg, string) {
	if !strings.HasPrefix(raw, "/") {
		return nil, "the path is not an absolute path"
	}
	parts := strings.Split(raw[1:], "/")
	segs := make([]patAPISeg, len(parts))
	for i, part := range parts {
		dec, err := url.PathUnescape(part)
		if err != nil || part == "" {
			return nil, "the path has an empty or undecodable segment"
		}
		if strings.ContainsFunc(dec, func(c rune) bool { return c < 0x20 || c == 0x7f || c == '\\' || c == ';' || c == '%' }) {
			return nil, "the path carries a backslash, semicolon, percent sign or control character after decoding"
		}
		for _, piece := range strings.Split(dec, "/") {
			if piece == "" || piece == "." || piece == ".." {
				return nil, "the path has an empty, \".\" or \"..\" segment"
			}
		}
		segs[i] = patAPISeg{s: dec, slash: strings.Contains(dec, "/")}
	}
	return segs, ""
}

// patAPIMatch finds the row admitting this method and path, returning the
// repository key too. Otherwise why says what kind of request it was.
func patAPIMatch(forge *patAPIForge, method string, segs []patAPISeg) (key string, row patAPIRow, tail []patAPISeg, why string) {
	key, tail, why = forge.repo(segs)
	if why != "" {
		// A request outside any repository is still named for what it is.
		if kind := patAPIRefusalKind(method, segs); kind != patAPIGeneric {
			why = kind
		}
		return "", patAPIRow{}, nil, why
	}
	for _, r := range forge.rows {
		if r.method == method && patAPITailMatches(strings.Split(r.tail, "/"), tail) {
			return key, r, tail, ""
		}
	}
	return "", patAPIRow{}, nil, patAPIRefusalKind(method, tail)
}

func patAPITailMatches(pattern []string, tail []patAPISeg) bool {
	for i, tok := range pattern {
		if tok == "**" {
			return i == len(pattern)-1 && len(tail) > i
		}
		if i >= len(tail) || !patAPITokenMatches(tok, tail[i]) {
			return false
		}
	}
	return len(tail) == len(pattern)
}

// patAPITokenMatches: "*" is one segment without an encoded "/"; "{f}" is one
// segment that may carry encoded "/" (a file path); "{n}" is digits; "{n}.diff"
// and "{n}.patch" are digits and that suffix; "{archive}" is "archive" with an
// optional archive extension; anything else is a literal.
func patAPITokenMatches(tok string, seg patAPISeg) bool {
	switch tok {
	case "*":
		return !seg.slash
	case "{f}":
		return true
	case "{n}":
		return patAPIDigits(seg.s)
	case "{archive}":
		ext, ok := strings.CutPrefix(seg.s, "archive")
		return ok && !seg.slash && (ext == "" || slices.Contains([]string{".zip", ".tar", ".tar.gz", ".tgz", ".tar.bz2", ".bz2"}, ext))
	}
	if suffix, ok := strings.CutPrefix(tok, "{n}"); ok {
		num, found := strings.CutSuffix(seg.s, suffix)
		return found && patAPIDigits(num)
	}
	return seg.s == tok && !seg.slash
}

func patAPIDigits(s string) bool {
	return s != "" && len(s) <= 18 && strings.Trim(s, "0123456789") == ""
}

// patAPIRefusalKind names why an unlisted request was refused, so the sandbox
// and the audit row read "merge is not available" rather than "no such row".
func patAPIRefusalKind(method string, tail []patAPISeg) string {
	has := func(names ...string) bool {
		return slices.ContainsFunc(tail, func(s patAPISeg) bool { return slices.Contains(names, strings.ToLower(s.s)) })
	}
	switch {
	case has("graphql"):
		return "GraphQL is not available through this door"
	case has("search"):
		return "search is not available through this door"
	case has("merge", "merge_ref", "rebase", "cancel_merge_when_pipeline_succeeds", "auto-merge", "auto_merge"):
		return "merging, rebasing and auto-merge are not available through this door"
	case method != http.MethodGet && (has("files", "contents", "commits", "browse", "raw") || (len(tail) > 0 && tail[0].s == "repository")):
		return "writing repository files or commits is not available through this door; push through the git broker so its content and branch rules apply"
	}
	return patAPIGeneric
}

const patAPIGeneric = "this request is not one of the operations this door admits"

// patAPIKeyOf is the repository key of a path whose leading segments name an
// owner and a repository, refusing a key PATRepoKey would not compare.
func patAPIKeyOf(forge, path string) (string, string) {
	key, ok := types.PATRepoKey(forge, path)
	if !ok || strings.HasSuffix(key, "/*") {
		return "", "the repository path is not one the grant can name"
	}
	return key, ""
}

// gitlabRepo reads /api/v4/projects/<url-encoded full path>/…. A project named
// by number cannot be mapped to a granted path, so it is refused.
func gitlabRepo(segs []patAPISeg) (string, []patAPISeg, string) {
	if len(segs) < 4 || segs[0].s != "api" || segs[1].s != "v4" || segs[2].s != "projects" {
		return "", nil, "this request is not under a repository"
	}
	proj := segs[3]
	if patAPIDigits(proj.s) {
		return "", nil, "a project named by its numeric id cannot be checked against the granted repositories; name it by its full path"
	}
	if !proj.slash {
		return "", nil, "the project must be named by its full URL-encoded path"
	}
	key, why := patAPIKeyOf(types.PATForgeGitLab, proj.s)
	return key, segs[4:], why
}

// giteaRepo reads /api/v1/repos/<owner>/<repo>/….
func giteaRepo(segs []patAPISeg) (string, []patAPISeg, string) {
	if len(segs) < 5 || segs[0].s != "api" || segs[1].s != "v1" || segs[2].s != "repos" || segs[3].slash || segs[4].slash {
		return "", nil, "this request is not under a repository"
	}
	key, why := patAPIKeyOf(types.PATForgeGitea, segs[3].s+"/"+segs[4].s)
	return key, segs[5:], why
}

// bitbucketRepo reads /rest/api/{1.0,latest}/projects/<key>/repos/<slug>/….
func bitbucketRepo(segs []patAPISeg) (string, []patAPISeg, string) {
	if len(segs) < 7 || segs[0].s != "rest" || segs[1].s != "api" || (segs[2].s != "1.0" && segs[2].s != "latest") ||
		segs[3].s != "projects" || segs[5].s != "repos" || segs[4].slash || segs[6].slash {
		return "", nil, "this request is not under a repository"
	}
	key, why := patAPIKeyOf(types.PATForgeBitbucketServer, segs[4].s+"/"+segs[6].s)
	return key, segs[7:], why
}
