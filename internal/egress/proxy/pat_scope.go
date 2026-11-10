// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A narrowed git_pat grant narrows the RUN: the PAT keeps the reach its issuer
// gave it, and this route refuses a request outside the grant's scope before
// anything can mint. Two checks, both run by handlePATBroker after the
// smart-HTTP verb check and before the push rules and patToken, so a refused
// request spends neither the credential nor an approval-gated single-use mint.
//
//   - repos: the repository the request names must be covered by an entry;
//   - access read: both doors of a push are shut, the ref advertisement
//     (GET info/refs?service=git-receive-pack) as well as the POST.

// patInfoRefs is the two-segment smart-HTTP tail handlePATBroker reads as one verb.
const patInfoRefs = "info/refs"

// scope returns the grant's narrowing axes with their omission defaults.
func (g PATGrant) scope() types.GitPATScope {
	return types.GitPATScope{Repos: g.Repos, Access: g.Access, Forge: g.Forge, API: g.API}.Normalize()
}

// validScope refuses a grant a control plane could not have written: an access
// or forge outside its enum, or a repos entry that is not a well-formed key for
// the forge. A proxy that carried it would enforce a different scope than the
// one dispatch meant, so it fails at start.
func (g PATGrant) validScope() error {
	switch g.Access {
	case "", types.PATAccessRead, types.PATAccessWrite:
	default:
		return fmt.Errorf("access %q is not one of read, write", g.Access)
	}
	switch g.Forge {
	case "", types.PATForgeGeneric, types.PATForgeGitLab, types.PATForgeBitbucketServer, types.PATForgeGitea:
	default:
		return fmt.Errorf("forge %q is not one of generic, gitlab, bitbucket_server, gitea", g.Forge)
	}
	if g.API && patAPIForges[g.scope().Forge] == nil {
		return fmt.Errorf("api is set for forge %q, which has no API table (gitlab, gitea, bitbucket_server)", g.scope().Forge)
	}
	if g.Repos != nil {
		for _, e := range *g.Repos {
			if _, ok := types.PATRepoKey(g.Forge, e); !ok {
				return fmt.Errorf("repos entry %q is malformed", e)
			}
		}
	}
	return nil
}

// validPATGrants applies validScope to every host's grant, at config load.
func validPATGrants(grants map[string]PATGrant) error {
	for host, g := range grants {
		if err := g.validScope(); err != nil {
			return fmt.Errorf("config: pat_grants[%q]: %w", host, err)
		}
	}
	return nil
}

// patRequestRepoKey reads the repository a smart-HTTP request names out of the
// upstream path rest ("/<repo path>/<tail>", tail being info/refs or the verb),
// with patGitRepo, and returns its key through types.PATRepoKey, the one
// repository-identity function. rest is the DECODED path, which is also the
// value forwardBrokeredGit rebuilds the upstream URL from, so the key compared
// here and the path forwarded are one string.
//
// No forge canonicalises: a form patGitRepo does not admit is refused, never
// guessed at. Bitbucket Server serves its repositories under
// [<context path>/]scm/<project>/<repo>, so what precedes <project> is part of
// the request form and not of the key.
func patRequestRepoKey(forge, rest, verb string) (string, bool) {
	repo, ok := patGitRepo(forge, rest, verb)
	if !ok {
		return "", false
	}
	key, ok := types.PATRepoKey(forge, repo)
	// A request names one concrete repository, never a wildcard.
	if !ok || strings.HasSuffix(key, "/*") {
		return "", false
	}
	return key, true
}

// patGitRepo judges rest ("/<repo path>/<tail>", tail the verb the caller
// validated) against the forge's git URL form and returns the repository path
// it names. Each forge mounts its API at a known place on the git host, so a
// path is refused when its repository part reaches one of those mounts, under
// any deployment prefix: whatever its last segment spells, it is a forge route
// the PAT must not reach. The repository's own name, the last segment, is never
// read as a mount. A doubly encoded path is refused, since a front end that
// decodes again would read another path: any decoded "%" on the named forges,
// whose names never hold one, and on generic a "%" before two hex digits
// (Azure DevOps names may hold a "%", as in "100% Done").
//
//   - every forge but bitbucket_server: patAPIMount;
//   - gitea also: "api" anywhere before the repository name (Gitea reserves
//     it as an owner, so there it is the API mount under a sub-path);
//   - generic also: "_api" (Azure DevOps). Gerrit needs no rule: with HTTP git
//     download enabled it binds its git servlet, which takes every
//     [a/]<project>/<git verb> path, ahead of its REST servlets (Daemon.java
//     GitOverHttpModule before WebModule). With HTTP git off, the broker
//     cannot clone from it at all.
//   - bitbucket_server: exactly [<context path>/]scm/<project>/<repo>, the
//     context path free of the server's own routes, and the repository is
//     <project>/<repo>.
func patGitRepo(forge, rest, verb string) (string, bool) {
	repo, ok := strings.CutSuffix(rest, "/"+verb)
	if forge == "" {
		forge = types.PATForgeGeneric
	}
	if !ok || patDoublyEncoded(forge, repo) {
		return "", false
	}
	segs := strings.Split(strings.TrimPrefix(repo, "/"), "/")
	// No clone URL has an empty segment, and one would move a mount off its
	// anchor ("//api/..."): refused, as is one a server reads as empty (";x").
	if slices.ContainsFunc(segs, func(s string) bool { return patSegRead(s) == "" }) {
		return "", false
	}
	repo = strings.Join(segs, "/")
	if forge == types.PATForgeBitbucketServer {
		return patBitbucketServerRepo(segs)
	}
	for i := range segs[:len(segs)-1] {
		if patAPIMount(segs, i) ||
			(forge == types.PATForgeGitea && patSegIs(segs[i], "api")) ||
			(forge == types.PATForgeGeneric && patSegIs(segs[i], "_api")) {
			return "", false
		}
	}
	return repo, true
}

// patDoublyEncoded reports whether a decoded repository path still holds a
// percent escape (patGitRepo).
func patDoublyEncoded(forge, repo string) bool {
	if forge != types.PATForgeGeneric {
		return strings.ContainsRune(repo, '%')
	}
	for i := strings.IndexByte(repo, '%'); i >= 0; i = strings.IndexByte(repo, '%') {
		if len(repo) > i+2 && strings.Trim(repo[i+1:i+3], "0123456789abcdefABCDEF") == "" {
			return true
		}
		repo = repo[i+1:]
	}
	return false
}

// patGitPath reports whether patGitRepo admits rest.
func patGitPath(forge, rest, verb string) bool {
	_, ok := patGitRepo(forge, rest, verb)
	return ok
}

// patAPIMount reports whether segs[i], which is not the repository's own name,
// starts a forge API mount:
//   - "api" first, or followed by a version (/api/v1 Gitea and Forgejo, /api/v3
//     and /api/graphql GitHub Enterprise, /api/v4 GitLab, under a sub-path too);
//   - "rest/<name>/<version>" (Bitbucket Server, /rest/api/1.0, /rest/git/latest);
//   - "!api" (Bitbucket Cloud), "_apis" (Azure DevOps), "-" (GitLab's route
//     scope; GitLab refuses it as a name at every level).
func patAPIMount(segs []string, i int) bool {
	s := segs[i]
	switch {
	case patSegIs(s, "api"):
		return i == 0 || (i+1 < len(segs)-1 && patAPIVersion(segs[i+1]))
	case patSegIs(s, "rest"):
		return i+2 < len(segs)-1 && patBitbucketVersion(segs[i+2])
	}
	return patSegIs(s, "!api") || patSegIs(s, "_apis") || patSegIs(s, "-")
}

// patBitbucketServerRoutes are the Bitbucket Server routes that take a free
// path after them; a context path holding one is not a context path.
var patBitbucketServerRoutes = []string{"scm", "rest", "plugins", "projects", "users"}

// patBitbucketServerRepo admits [<context path>/]scm/<project>/<repo> and returns
// <project>/<repo>.
func patBitbucketServerRepo(segs []string) (string, bool) {
	n := len(segs)
	if n < 3 || segs[n-3] != "scm" || segs[n-2] == "" || segs[n-1] == "" {
		return "", false
	}
	for i, s := range segs[:n-3] {
		if s == "" || patAPIMount(segs[:n-3], i) ||
			slices.ContainsFunc(patBitbucketServerRoutes, func(r string) bool { return patSegIs(s, r) }) {
			return "", false
		}
	}
	return segs[n-2] + "/" + segs[n-1], true
}

// patSegIs reports whether a decoded path segment reads as word to a server
// that drops ";" path parameters, trims trailing dots and spaces, and matches
// routes case-insensitively, under any case mapping that lands on ASCII:
// "ſ" (U+017F) reads as "s", "K" (U+212A) as "k", "ı" (U+0131) and "İ"
// (U+0130) as "i".
func patSegIs(seg, word string) bool {
	return strings.EqualFold(strings.Map(patASCIICase, patSegRead(seg)), word)
}

// patASCIICase maps a non-ASCII rune to the ASCII letter its upper or lower
// case is, if either is one.
func patASCIICase(r rune) rune {
	if r < utf8.RuneSelf {
		return r
	}
	if u := unicode.ToUpper(r); u < utf8.RuneSelf {
		return u
	}
	if l := unicode.ToLower(r); l < utf8.RuneSelf {
		return l
	}
	return r
}

func patSegRead(seg string) string {
	seg, _, _ = strings.Cut(seg, ";")
	return strings.TrimRight(seg, ". ")
}

// patAPIVersion: v<digits> or graphql, as patSegIs reads a segment.
func patAPIVersion(seg string) bool {
	s := patSegRead(seg)
	if strings.EqualFold(s, "graphql") {
		return true
	}
	return len(s) > 1 && (s[0] == 'v' || s[0] == 'V') && strings.Trim(s[1:], "0123456789") == ""
}

// patBitbucketVersion: <digits>.<digits> or latest, as patSegIs reads a segment.
func patBitbucketVersion(seg string) bool {
	s := patSegRead(seg)
	if strings.EqualFold(s, "latest") {
		return true
	}
	major, minor, ok := strings.Cut(s, ".")
	return ok && major != "" && minor != "" && strings.Trim(major+minor, "0123456789") == ""
}

// patRepoAllowed reports whether the grant's repos cover the repository the
// request names. A grant that sets no repos allows every repository.
func patRepoAllowed(g PATGrant, rest, verb string) bool {
	if g.Repos == nil {
		return true
	}
	key, ok := patRequestRepoKey(g.scope().Forge, rest, verb)
	if !ok {
		return false
	}
	return slices.ContainsFunc(*g.Repos, func(entry string) bool { return types.PATRepoCovers(entry, key) })
}

// patReadOnlyRefuses reports whether a read-only grant refuses the request: the
// push POST, and the ref advertisement a push starts with. Refusing only the
// POST would still advertise refs for a push the run can never make.
func patReadOnlyRefuses(g PATGrant, verb, service string) bool {
	if g.scope().Access != types.PATAccessRead {
		return false
	}
	return verb == "git-receive-pack" || (verb == patInfoRefs && service == "git-receive-pack")
}

// patScopeAllows applies the grant's repos and read-only axes. On false it has
// already answered the sandbox with a 403 and a decision row naming the axis.
func (p *Proxy) patScopeAllows(w http.ResponseWriter, r *http.Request, host, rest, verb string, g PATGrant) bool {
	if !patRepoAllowed(g, rest, verb) {
		p.emitPATDecision(r, host, egress.Deny, ruleSourcePATRepo)
		http.Error(w, "repository not granted to this run", http.StatusForbidden)
		return false
	}
	if patReadOnlyRefuses(g, verb, r.URL.Query().Get("service")) {
		p.emitPATDecision(r, host, egress.Deny, ruleSourcePATReadOnly)
		http.Error(w, "this run's git_pat grant is read-only", http.StatusForbidden)
		return false
	}
	return true
}
