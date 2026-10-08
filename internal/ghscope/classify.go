// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ghscope

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// The two hosts this catalogue classifies. GitHub Enterprise Server, the
// upload, raw-content and package hosts are absent on purpose: a request to
// any other host is an error.
const (
	// APIHost serves the REST API.
	APIHost = "api.github.com"
	// GitHost serves git over HTTP; nothing else on it is classified.
	GitHost = "github.com"
)

// Request is ONE candidate GitHub request.
type Request struct {
	// Method is the HTTP method as it arrived on the request line.
	Method string
	// Host is the request's host WITHOUT a port.
	Host string
	// Path is the request's path, still percent-encoded and WITHOUT the query string.
	Path string
	// RawQuery is the request's query string as it arrived. Only the git
	// handshake reads it: that route names its service there.
	RawQuery string
	// Header may be nil. Only the method-override header is read, and it refuses.
	Header http.Header
}

// Verdict is one classification: the access the request needs and what it
// targets. Permits reads all three fields; a caller gating on Capability
// alone has dropped the repository pin.
type Verdict struct {
	// Capability is the ONE access this request needs.
	Capability Capability
	// Repo is the repository the request targets, as RepoKey spells it
	// ("owner/name"), or "" when its path names none.
	Repo string
	// Owner is the account or organisation the path names — Repo's owner, or
	// the organisation of an organisation route — or "" when it names none.
	Owner string
}

// Classify names the ONE capability req needs and the repository it targets,
// or refuses req. It errors only where no capability could be honest: a host
// it does not know, a method it does not classify, a method-override header,
// a path whose spelling GitHub might route differently from how it reads, an
// owner or repository segment that is not a name, and a git handshake that
// does not say which service it starts. Every other request classifies, and
// one this catalogue does not recognize classifies as CapUnclassifiedRead or
// CapUnclassifiedWrite, which nobody holds.
func Classify(req Request) (Verdict, error) {
	read, method, err := methodOf(req)
	if err != nil {
		return Verdict{}, err
	}
	segs, err := splitPath(req.Path)
	if err != nil {
		return Verdict{}, err
	}
	switch strings.ToLower(strings.TrimSuffix(strings.TrimSpace(req.Host), ".")) {
	case APIHost:
		return classifyREST(read, method, segs)
	case GitHost:
		return classifyGit(method, segs, req.RawQuery)
	}
	return Verdict{}, fmt.Errorf("ghscope: %q is not a GitHub host this catalogue classifies", req.Host)
}

// methodOf is the request's method and whether it is a read. HEAD reads as
// GET. OPTIONS, TRACE and CONNECT are not API calls and are refused. So is any
// request carrying X-HTTP-Method-Override: whether GitHub honours it is not
// documented, so the method the server would act on is not knowable.
func methodOf(req Request) (read bool, method string, err error) {
	for k := range req.Header {
		if strings.EqualFold(k, "X-HTTP-Method-Override") {
			return false, "", fmt.Errorf("ghscope: the request carries X-HTTP-Method-Override — the method the server acts on is not knowable")
		}
	}
	switch method = strings.ToUpper(strings.TrimSpace(req.Method)); method {
	case http.MethodGet, http.MethodHead:
		return true, http.MethodGet, nil
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return false, method, nil
	}
	return false, "", fmt.Errorf("ghscope: %q is not an HTTP method this catalogue classifies", req.Method)
}

// unclassified is the fail-closed answer for a route no rule names.
func unclassified(read bool) Capability {
	if read {
		return CapUnclassifiedRead
	}
	return CapUnclassifiedWrite
}

// texts is the decoded text of each segment.
func texts(segs []segment) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = s.text
	}
	return out
}

// at is p[i], or "" past the end. EVERY route rule indexes through it rather
// than searching segments: a branch named "hooks" by its creator must not
// make a read of it look like a webhook.
func at(p []string, i int) string {
	if i < len(p) {
		return p[i]
	}
	return ""
}

// deniedRoots are the top-level areas refused for every method: each is a
// door onto a credential.
var deniedRoots = []string{"app", "app-manifests", "applications", "authorizations", "credentials", "installation"}

// metadataRoots are the top-level reads that return no account's data.
var metadataRoots = []string{"meta", "octocat", "rate_limit", "versions", "zen"}

// accountRoots are the top-level areas that span the person's whole reach.
// /repositories/{id} is a numeric alias for a repository this catalogue
// cannot pin to a name.
var accountRoots = []string{"gists", "notifications", "repositories"}

// classifyREST routes one api.github.com request by its first segment.
// DEFAULT is unclassified: a top-level area is refused until someone adds it
// here — the numeric /organizations alias, /teams, /enterprises, and
// everything GitHub adds later.
func classifyREST(read bool, method string, segs []segment) (Verdict, error) {
	p := texts(segs)
	if len(p) == 0 {
		if read {
			return Verdict{Capability: CapMetadata}, nil
		}
		return Verdict{Capability: CapUnclassifiedWrite}, nil
	}
	switch p[0] {
	case "repos":
		return classifyRepo(read, method, segs[1:])
	case "orgs":
		return classifyOrg(read, method, segs[1:])
	case "user":
		return classifyUser(read, segs[1:])
	case "users":
		return classifyUsers(read, method, segs[1:])
	case "graphql":
		return Verdict{Capability: CapDeniedGraphQL}, nil
	case "search":
		return Verdict{Capability: CapDeniedSearch}, nil
	}
	switch {
	case slices.Contains(deniedRoots, p[0]):
		return Verdict{Capability: CapDeniedTokens}, nil
	case slices.Contains(accountRoots, p[0]):
		return Verdict{Capability: CapDeniedAccount}, nil
	case read && len(p) == 1 && slices.Contains(metadataRoots, p[0]):
		return Verdict{Capability: CapMetadata}, nil
	}
	return Verdict{Capability: unclassified(read)}, nil
}

// classifyUser is the signed-in person's own account. Reading the profile
// itself is CapIdentity; everything else under /user — every write, and their
// keys, emails, installations, organisations, packages and the list of every
// repository they can reach — is CapDeniedAccount.
func classifyUser(read bool, segs []segment) (Verdict, error) {
	if read && len(segs) == 0 {
		return Verdict{Capability: CapIdentity}, nil
	}
	return Verdict{Capability: CapDeniedAccount}, nil
}

// classifyUsers is /users/{username}/…: only its packages are classified,
// with Owner set so a caller holds them to the owners of the run's
// repositories. Everything else about another account is unclassified.
func classifyUsers(read bool, method string, segs []segment) (Verdict, error) {
	if len(segs) < 2 || segs[1].text != "packages" {
		return Verdict{Capability: unclassified(read)}, nil
	}
	owner, ok := ownerName(segs[0])
	if !ok {
		return Verdict{}, fmt.Errorf("ghscope: %q is not an account name", segs[0].text)
	}
	return Verdict{Capability: packagesCapability(read, method, texts(segs[2:])), Owner: owner}, nil
}

// packagesCapability is …/packages/{type}/{name}[/versions/{id}][/restore]
// under an organisation or an account: every read is CapPackagesRead; a
// DELETE of a package or a version, and a POST restoring one, is
// CapPackagesWrite.
func packagesCapability(read bool, method string, sub []string) Capability {
	if read {
		return CapPackagesRead
	}
	n := len(sub)
	switch {
	case method == http.MethodDelete && (n == 2 || n == 4 && sub[2] == "versions"):
		return CapPackagesWrite
	case method == http.MethodPost && (n == 3 || n == 5 && sub[2] == "versions") && sub[n-1] == "restore":
		return CapPackagesWrite
	}
	return CapUnclassifiedWrite
}

// orgMemberAreas are the organisation resources about who belongs to it: a
// read of one is CapOrgRead, a write CapOrgAdmin.
var orgMemberAreas = []string{"invitations", "members", "memberships", "outside_collaborators", "public_members", "teams"}

// classifyOrg is /orgs/{org}/…, with Owner set so a caller can hold the
// request to the organisations of the run's own repositories.
func classifyOrg(read bool, method string, segs []segment) (Verdict, error) {
	if len(segs) == 0 {
		return Verdict{Capability: unclassified(read)}, nil
	}
	org, ok := ownerName(segs[0])
	if !ok {
		return Verdict{}, fmt.Errorf("ghscope: %q is not an organisation name", segs[0].text)
	}
	return Verdict{Capability: orgCapability(read, method, texts(segs[1:])), Owner: org}, nil
}

func orgCapability(read bool, method string, sub []string) Capability {
	if c, ok := deniedOrgArea(sub); ok {
		return c
	}
	switch {
	case len(sub) == 0 && read:
		return CapOrgRead
	case len(sub) == 0 && method == http.MethodPatch:
		// DELETE here deletes the organisation; no run does that.
		return CapOrgAdmin
	case len(sub) == 1 && sub[0] == "repos" && read:
		return CapOrgRead
	case len(sub) == 1 && sub[0] == "repos" && method == http.MethodPost:
		return CapRepoAdmin
	case at(sub, 0) == "packages":
		return packagesCapability(read, method, sub[1:])
	case len(sub) > 0 && slices.Contains(orgMemberAreas, sub[0]):
		if read {
			return CapOrgRead
		}
		return CapOrgAdmin
	}
	return unclassified(read)
}

// secretAreas are the areas that hold a secrets resource one level down.
var secretAreas = []string{"actions", "codespaces", "dependabot"}

// deniedOrgArea is the organisation half of the refusal table, checked BEFORE
// the method: a GET of the token area is a read of exactly the thing no lane
// may see.
func deniedOrgArea(sub []string) (Capability, bool) {
	switch a := at(sub, 0); {
	case a == "hooks":
		return CapDeniedHooks, true
	case slices.Contains([]string{"credential-authorizations", "installation", "installations",
		"personal-access-token-requests", "personal-access-tokens"}, a):
		return CapDeniedTokens, true
	case a == "actions" && at(sub, 1) == "runners":
		return CapDeniedTokens, true
	case a == "private-registries", slices.Contains(secretAreas, a) && at(sub, 1) == "secrets":
		return CapDeniedSecrets, true
	}
	return "", false
}

// classifyRepo is /repos/{owner}/{repo}/…. Repo and Owner are set on every
// verdict it returns, refusals included, so a caller holds the request to the
// run's repositories whatever it classifies as.
func classifyRepo(read bool, method string, segs []segment) (Verdict, error) {
	if len(segs) < 2 {
		return Verdict{Capability: unclassified(read)}, nil
	}
	owner, repo, err := repoOf(segs[0], segs[1])
	if err != nil {
		return Verdict{}, err
	}
	return Verdict{Capability: repoCapability(read, method, texts(segs[2:])), Repo: owner + "/" + repo, Owner: owner}, nil
}

// repoOf reads the two segments that name a repository.
func repoOf(o, r segment) (owner, repo string, err error) {
	owner, ok := ownerName(o)
	if !ok {
		return "", "", fmt.Errorf("ghscope: %q is not an account or organisation name", o.text)
	}
	repo, ok = repoName(r)
	if !ok {
		return "", "", fmt.Errorf("ghscope: %q is not a repository name", r.text)
	}
	return owner, repo, nil
}

// gitPackServices are the two smart-HTTP services and what each needs.
var gitPackServices = map[string]Capability{
	"git-upload-pack":  CapCodeRead,
	"git-receive-pack": CapCodeWrite,
}

// classifyGit is git over HTTP on GitHost: /{owner}/{repo}[.git]/info/refs
// naming its service in the query, then a POST to that service. A fetch is
// CapCodeRead and a push CapCodeWrite; WHICH refs a push moves is in a pack
// this catalogue does not parse, and is the git door's to hold. The dumb
// protocol (a handshake naming no service), LFS, and everything else on the
// host is not classified.
func classifyGit(method string, segs []segment, rawQuery string) (Verdict, error) {
	p := texts(segs)
	var service string
	switch {
	case len(p) == 4 && p[2] == "info" && p[3] == "refs":
		if method != http.MethodGet {
			return Verdict{}, fmt.Errorf("ghscope: a git handshake is a GET")
		}
		s, ok := strings.CutPrefix(rawQuery, "service=")
		if !ok {
			return Verdict{}, fmt.Errorf("ghscope: a git handshake that does not name exactly one service cannot be classified")
		}
		service = s
	case len(p) == 3:
		if _, known := gitPackServices[p[2]]; !known {
			return Verdict{Capability: unclassified(method == http.MethodGet)}, nil
		}
		if method != http.MethodPost {
			return Verdict{}, fmt.Errorf("ghscope: a git %s request is a POST", p[2])
		}
		service = p[2]
	default:
		return Verdict{Capability: unclassified(method == http.MethodGet)}, nil
	}
	c, ok := gitPackServices[service]
	if !ok {
		return Verdict{}, fmt.Errorf("ghscope: %q is not a git service this catalogue classifies", service)
	}
	owner, repo, err := repoOf(segs[0], segs[1])
	if err != nil {
		return Verdict{}, err
	}
	return Verdict{Capability: c, Repo: owner + "/" + repo, Owner: owner}, nil
}
