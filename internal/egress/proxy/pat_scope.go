// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

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
// with the forge's path table, and returns its key through types.PATRepoKey, the
// one repository-identity function. rest is the DECODED path, which is also the
// value forwardBrokeredGit rebuilds the upstream URL from, so the key compared
// here and the path forwarded are one string.
//
// No forge canonicalises: a form the table does not list (an empty repository,
// a Bitbucket Server path outside /scm/) is refused, never guessed at. Bitbucket
// Server serves its repositories under /scm/<project>/<repo>, so that prefix is
// part of the request form and not of the key.
func patRequestRepoKey(forge, rest, verb string) (string, bool) {
	repo, ok := strings.CutSuffix(rest, "/"+verb)
	if !ok {
		return "", false
	}
	repo = strings.TrimPrefix(repo, "/")
	if forge == types.PATForgeBitbucketServer {
		if repo, ok = strings.CutPrefix(repo, "scm/"); !ok {
			return "", false
		}
	}
	key, ok := types.PATRepoKey(forge, repo)
	// A request names one concrete repository, never a wildcard.
	if !ok || strings.HasSuffix(key, "/*") {
		return "", false
	}
	return key, true
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
