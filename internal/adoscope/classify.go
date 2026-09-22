// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Request is ONE candidate Azure DevOps request, as much of it as a classifier
// can be given without buffering the whole body.
//
// It is a struct rather than a parameter list because two of the hazards need
// more than the wire fields: an organisation to pin the path against, and an
// oracle for "is this ref protected". Both are properties of the ROW and the
// RUN, not of the request, and neither can be guessed from the bytes.
type Request struct {
	// Method is the HTTP method as it arrived on the request line. An
	// override header may RAISE it — see Classify.
	Method string
	// Host is the request's host WITHOUT a port: a caller holding
	// "dev.azure.com:443" strips the port before it gets here, because a port
	// would make the host match nothing and the request would be refused as
	// not Azure DevOps.
	Host string
	// Path is the request's path, still percent-encoded and WITHOUT the query
	// string. A query left on it would become a final path segment and could
	// only ever make the route read as something it is not.
	Path string
	// Header is the request's headers. May be nil. The keys are compared
	// case-insensitively here, so a map built by hand — or one that preserved
	// the wire spelling instead of canonicalising it — is read the same way a
	// net/http server's is.
	Header http.Header
	// BodyPeek is the first MaxBodyPeek bytes of the body, or nil when there
	// is none. A route that needs the body and cannot see all of it is
	// REFUSED, never classified on the visible prefix.
	BodyPeek []byte
	// Org is the organisation the provider row pinned. REQUIRED: an
	// unpinned request is refused, because "some Azure DevOps organisation"
	// is not a policy.
	Org string
	// RefProtected answers whether one ref is covered by a branch policy. It
	// is given the ref name EXACTLY as the request body spelled it, which on
	// every documented Azure DevOps ref route is the full name
	// ("refs/heads/main") and not a short one ("main"); an oracle whose cache
	// is keyed differently normalizes on its own side.
	//
	// Optional — when nil, a ref move classifies as CapCodeWrite and the
	// caller is handed the ref names in Verdict.Refs so it can re-decide
	// against its own per-run cache.
	RefProtected func(ref string) bool
}

// Verdict is one classification.
type Verdict struct {
	// Capability is the ONE access this request needs.
	Capability Capability
	// Refs are the ref names the request's body named, if any. Always
	// returned for a ref move, whether or not RefProtected was supplied: the
	// caller that keeps the protected-branch cache is the one that can answer
	// cheaply, and it cannot ask about refs it was never told.
	Refs []string
}

// readMethods are the methods that cannot change anything.
var readMethods = []string{http.MethodGet, http.MethodHead, http.MethodOptions}

// writeMethods are the methods that can.
var writeMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// Classify names the ONE capability req needs, or refuses req.
//
// It refuses — returns an error — only where no capability could be honest: a
// host that is not Azure DevOps, an organisation that is not the row's, a path
// whose structure is hidden behind percent-encoding or dot segments, a method
// override outside its documented shape, a git-over-HTTP endpoint, or a body
// the peek cannot see whole. Everything else classifies, and a write it does
// not recognize classifies as CapUnclassifiedWrite, which is not grantable.
//
// GIT-OVER-HTTP IS OUT OF SCOPE and is refused by name rather than classified.
// The ref names a push carries live in a pack protocol this catalogue does not
// parse, so no answer here could tell a clone from a push onto a protected
// branch; both used to land on CapUnclassifiedWrite, which told a caller
// nothing about which it had. The transport is gated on its own.
func Classify(req Request) (Verdict, error) {
	method, err := effectiveMethod(req.Method, req.Header)
	if err != nil {
		return Verdict{}, err
	}
	r, err := parseRoute(req.Host, req.Path, req.Org)
	if err != nil {
		return Verdict{}, err
	}
	// The denied areas are checked BEFORE the method: a GET of the token area
	// lists an organisation's personal access tokens, which is a read of
	// exactly the thing no lane may see.
	if c, ok := deniedAreas[r.area]; ok {
		return Verdict{Capability: c}, nil
	}
	if slices.Contains(readMethods, method) || readWrite(r) {
		return Verdict{Capability: CapRead}, nil
	}
	return classifyWrite(method, r, req)
}

// effectiveMethod is the method the SERVER will act on, which is not always
// the one on the request line: Azure DevOps honours X-HTTP-Method-Override, so
// a POST carrying `X-HTTP-Method-Override: PATCH` is a PATCH.
//
// AN OVERRIDE MAY ONLY RAISE. Microsoft documents the header for a POST
// carrying PATCH or DELETE — a tunnel for clients that cannot send those verbs
// — and says nothing about a POST carrying GET. Honouring a downward override
// meant a POSTed push classified as a READ on the word of a header the caller
// controls. Refusing is the same answer this package gives every other "which
// of the two will the server act on" question: the header is accepted on a
// POST only, it must name a write, and anything else is refused, not ignored.
func effectiveMethod(method string, h http.Header) (string, error) {
	base := strings.ToUpper(strings.TrimSpace(method))
	if !knownMethod(base) {
		return "", fmt.Errorf("adoscope: %q is not an HTTP method this catalogue classifies", method)
	}
	ov := headerValues(h, "X-HTTP-Method-Override")
	switch {
	case len(ov) == 0:
		return base, nil
	case len(ov) > 1:
		return "", fmt.Errorf("adoscope: %d X-HTTP-Method-Override values — which one the server acts on is not knowable", len(ov))
	case base != http.MethodPost:
		return "", fmt.Errorf("adoscope: X-HTTP-Method-Override on a %s — the header is documented on a POST only", base)
	}
	over := strings.ToUpper(strings.TrimSpace(ov[0]))
	switch {
	case !knownMethod(over):
		return "", fmt.Errorf("adoscope: X-HTTP-Method-Override: %q is not an HTTP method", ov[0])
	case !slices.Contains(writeMethods, over):
		return "", fmt.Errorf("adoscope: X-HTTP-Method-Override: %q would lower a POST to a read — an override may only raise", ov[0])
	}
	return over, nil
}

// headerValues is h's values for name, matched CASE-INSENSITIVELY against the
// map's own keys.
//
// http.Header.Values canonicalises the key it is GIVEN but not the keys
// already in the map, so a header map built by hand — or one that kept the
// wire spelling — hid "x-http-method-override" from this package completely.
// Reading the map directly is the only spelling that cannot be fooled by the
// caller's choice of capitalisation.
func headerValues(h http.Header, name string) []string {
	var out []string
	for k, v := range h {
		if strings.EqualFold(k, name) {
			out = append(out, v...)
		}
	}
	return out
}

// knownMethod reports whether m is a method this catalogue has an opinion
// about. TRACE and CONNECT are deliberately absent: neither is an Azure DevOps
// API call, so neither has a capability.
func knownMethod(m string) bool {
	return slices.Contains(readMethods, m) || slices.Contains(writeMethods, m)
}

// route is one request's path, decoded and split at the _apis boundary.
type route struct {
	// host is the lowercased host.
	host string
	// segs is every decoded, lowercased path segment after the organisation.
	segs []string
	// apis is the index of "_apis" in segs, or -1.
	apis int
	// area is the segment after _apis ("git", "wit", …), or "".
	area string
	// res is the segment after the area, or "".
	res string
}

// at is the segment n positions after _apis — at(1) is the area, at(2) the
// resource, at(3) the resource's id, and so on — or "".
//
// EVERY route rule indexes through this rather than searching the segments,
// and that is the fix for a whole class of evasion: a repository is named by
// whoever created it, so a repository called "pullrequestquery" made every
// write on it read as a query, and one called "items" made repository DELETION
// read as a code write. A resource word counts only where the documented route
// puts it.
func (r route) at(n int) string {
	if r.apis < 0 {
		return ""
	}
	if i := r.apis + n; i >= 0 && i < len(r.segs) {
		return r.segs[i]
	}
	return ""
}

// parseRoute decodes path, pins it to org, and splits it at _apis.
//
// The DECODING is the security-relevant half, and it refuses three things
// rather than resolving them:
//
//   - percent-encoding that hides the structure: matching the raw path would
//     let "%5Fapis/tokens" slip past the denied-area check into the read floor;
//   - a segment that decodes to contain a separator: "a%2Fb" is one segment to
//     the router and two to the reader, and either answer is wrong for the
//     other;
//   - a "." or ".." segment. Azure DevOps RESOLVES these server-side, so
//     "/acme/_apis/wit/../hooks/subscriptions" is a service-hook write that
//     reads here as a work-item write, and "/acme/../evil/…" leaves the
//     organisation the row pinned entirely. Refusing is both simpler than RFC
//     3986 resolution and strictly safer — nothing legitimate on this API
//     needs one.
func parseRoute(host, rawPath, org string) (route, error) {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if !azureDevOpsHost(h) {
		return route{}, fmt.Errorf("adoscope: %q is not an Azure DevOps host", host)
	}
	if strings.TrimSpace(org) == "" {
		return route{}, fmt.Errorf("adoscope: no organisation pinned — an unpinned request cannot be classified")
	}
	segs, err := decodeSegments(rawPath)
	if err != nil {
		return route{}, err
	}
	if slices.Contains(segs, "_git") {
		return route{}, fmt.Errorf("adoscope: %q is a git-over-HTTP endpoint — the transport is gated on its own, not classified here", rawPath)
	}
	segs, err = pinOrg(h, segs, org)
	if err != nil {
		return route{}, err
	}
	return splitAtAPIs(h, segs), nil
}

// azureDevOpsHost reports whether h is one of the hosts that accept Entra
// tokens: dev.azure.com and its service subdomains (vssps, vsrm, almsearch,
// feeds, pkgs, extmgmt), or a legacy <org>.visualstudio.com. An Azure DevOps
// SERVER host is absent on purpose — it does not accept Entra tokens at all,
// which is the same rule the provider row's write boundary enforces.
func azureDevOpsHost(h string) bool {
	return h == "dev.azure.com" || strings.HasSuffix(h, ".dev.azure.com") ||
		strings.HasSuffix(h, ".visualstudio.com")
}

// decodeSegments splits rawPath into decoded, lowercased, non-empty segments,
// refusing the shapes parseRoute documents.
func decodeSegments(rawPath string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(rawPath, "/") {
		if raw == "" {
			continue
		}
		seg, err := unescapeSegment(raw)
		if err != nil {
			return nil, err
		}
		if seg == "." || seg == ".." {
			return nil, fmt.Errorf("adoscope: path segment %q is a dot segment — the service resolves it to a different route", raw)
		}
		out = append(out, seg)
	}
	return out, nil
}

// unescapeSegment percent-decodes one segment and lowercases it, refusing a
// segment that decodes into a separator.
//
// It decodes by hand rather than through url.PathUnescape for one reason:
// PathUnescape leaves an encoded "/" as a literal slash in its output, which
// then has to be re-detected, and the two-step version of that rule is exactly
// where a laundering bug hides. Here the byte is refused where it is decoded.
func unescapeSegment(raw string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '%' {
			b.WriteByte(raw[i])
			continue
		}
		if i+2 >= len(raw) {
			return "", fmt.Errorf("adoscope: path segment %q is not decodable", raw)
		}
		hi, lo := unhex(raw[i+1]), unhex(raw[i+2])
		if hi < 0 || lo < 0 {
			return "", fmt.Errorf("adoscope: path segment %q is not decodable", raw)
		}
		c := byte(hi<<4 | lo)
		if c == '/' || c == '\\' {
			return "", fmt.Errorf("adoscope: path segment %q decodes to a second segment", raw)
		}
		b.WriteByte(c)
		i += 2
	}
	return strings.ToLower(b.String()), nil
}

// unhex is one hex digit's value, or -1.
func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// pinOrg checks the request's organisation against the row's and returns the
// segments AFTER it. On dev.azure.com the organisation is the first path
// segment; on a legacy visualstudio.com host it is the first host label and
// the path carries none. The comparison is case-insensitive because Azure
// DevOps' own URLs are.
func pinOrg(host string, segs []string, org string) ([]string, error) {
	want := strings.ToLower(strings.TrimSpace(org))
	if strings.HasSuffix(host, ".visualstudio.com") {
		if label, _, _ := strings.Cut(host, "."); label != want {
			return nil, fmt.Errorf("adoscope: host names organisation %q, row pins %q", label, org)
		}
		return segs, nil
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("adoscope: path names no organisation, row pins %q", org)
	}
	if segs[0] != want {
		return nil, fmt.Errorf("adoscope: path names organisation %q, row pins %q", segs[0], org)
	}
	return segs[1:], nil
}

// splitAtAPIs finds the _apis boundary and names the area and resource after
// it. A path with no _apis (the web UI) yields an empty area, which reads as
// the fail-closed answer for a write.
func splitAtAPIs(host string, segs []string) route {
	r := route{host: host, segs: segs, apis: slices.Index(segs, "_apis")}
	r.area, r.res = r.at(1), r.at(2)
	return r
}

// deniedAreas is the area -> refusal table. These are refused for EVERY
// method, including reads.
//
// The token family is deliberately WIDE: "tokens" is only the documented PAT
// lifecycle door, while the session-token, delegated-authorization and
// web-platform-auth areas each hand out a credential of their own. A lane that
// can obtain a second credential is a lane that can leave the lane, whichever
// area it left through.
var deniedAreas = map[string]Capability{
	"tokens":              CapDeniedTokens,
	"token":               CapDeniedTokens,
	"tokenadmin":          CapDeniedTokens,
	"tokenadministration": CapDeniedTokens,
	"accesstokens":        CapDeniedTokens,
	"delegatedauth":       CapDeniedTokens,
	"webplatformauth":     CapDeniedTokens,
	"hooks":               CapDeniedServiceHooks,
	"servicehooks":        CapDeniedServiceHooks,
	"extensionmanagement": CapDeniedExtensions,
	"gallery":             CapDeniedExtensions,
	"contribution":        CapDeniedInternal,
}

// readWrite reports whether r is one of the POSTs that only READ. Azure DevOps
// uses POST for queries whose input is too big for a query string, so treating
// method as intent would charge a work-item query the same capability as a
// work-item edit — and an operator who granted "read" would watch reads fail.
//
// The list is closed, short and POSITIONAL: every entry is a documented query
// endpoint matched where the route puts it, never wherever the word appears.
// _apis/Contribution/HierarchyQuery is NOT here — it is a query too, but one
// that reads every area at once, and it is refused as a denied area before
// this is reached.
func readWrite(r route) bool {
	switch r.area {
	case "wit":
		return r.res == "wiql" || r.res == "workitemsbatch"
	case "git":
		// {project}/_apis/git/repositories/{repositoryId}/pullrequestquery
		return r.res == "repositories" && r.at(4) == "pullrequestquery"
	case "search":
		// Code/work-item search lives on its own host; the area name alone is
		// not enough to trust, since "search" under dev.azure.com is not a
		// documented query door.
		return strings.HasPrefix(r.host, "almsearch.")
	}
	return false
}

// classifyWrite names the capability a write needs, by area.
//
// The DEFAULT is CapUnclassifiedWrite, and every branch that cannot name its
// capability falls into it. That is the whole fail-closed contract: a new
// Azure DevOps area appearing in a future API version is refused until someone
// adds it here, rather than inheriting whatever the nearest area was granted.
func classifyWrite(method string, r route, req Request) (Verdict, error) {
	switch r.area {
	case "wit":
		return witWrite(r, req)
	case "git":
		return gitWrite(method, r, req)
	case "policy":
		return Verdict{Capability: CapPolicyAdmin}, nil
	case "accesscontrollists", "accesscontrolentries", "securitynamespaces",
		"securityroles", "permissions", "identities", "graph":
		return Verdict{Capability: CapSecurityAdmin}, nil
	case "serviceendpoint":
		return Verdict{Capability: CapServiceEndpointAdmin}, nil
	case "build":
		return Verdict{Capability: buildWrite(r)}, nil
	case "pipelines":
		return Verdict{Capability: pipelinesWrite(r)}, nil
	case "release":
		return Verdict{Capability: releaseWrite(r)}, nil
	case "wiki":
		return Verdict{Capability: CapWikiWrite}, nil
	case "packaging", "packages":
		return Verdict{Capability: CapPackagingWrite}, nil
	case "projects", "projectcollections":
		return Verdict{Capability: CapProjectAdmin}, nil
	}
	return Verdict{Capability: CapUnclassifiedWrite}, nil
}

// witWrite is the work-item area. The $batch door is the interesting one — see
// batchIsWorkItemsOnly.
func witWrite(r route, req Request) (Verdict, error) {
	if r.res == "$batch" {
		if err := batchIsWorkItemsOnly(req); err != nil {
			return Verdict{}, err
		}
	}
	return Verdict{Capability: CapWorkWrite}, nil
}

// gitCodeWriteResources are the git sub-resources that change code without
// touching a branch policy or the repository object itself.
var gitCodeWriteResources = []string{
	"items", "commits", "merges", "cherrypicks", "reverts",
	"annotatedtags", "importrequests", "forksyncrequests", "suggestions",
}

// gitWrite is the git area, which carries four different capabilities behind
// one scope: the repository object (CapRepoAdmin), its policies
// (CapPolicyAdmin), its pull requests (CapPR) and its refs (CapCodeWrite or
// CapPolicyBypass, decided by the body).
//
// The split is POSITIONAL — _apis/git/{resource}/… — because the only other
// way to read it is to search the segments, and one of those segments is a
// repository name the caller chose.
func gitWrite(method string, r route, req Request) (Verdict, error) {
	switch r.res {
	case "repositories":
		return gitRepositoryWrite(method, r, req)
	case "pullrequests":
		// The organisation/project-level pull-request route.
		return pullRequestWrite(method, req)
	case "policy", "policyconfigurations":
		return Verdict{Capability: CapPolicyAdmin}, nil
	}
	return Verdict{Capability: CapUnclassifiedWrite}, nil
}

// gitRepositoryWrite splits _apis/git/repositories/{repositoryId}/{resource}.
// With no {resource} the request is about the repository OBJECT itself —
// creating, renaming or deleting it — whatever the repository happens to be
// called.
func gitRepositoryWrite(method string, r route, req Request) (Verdict, error) {
	switch res := r.at(4); res {
	case "":
		return Verdict{Capability: CapRepoAdmin}, nil
	case "refs", "pushes":
		return refWrite(req)
	case "pullrequests":
		return pullRequestWrite(method, req)
	case "policyconfigurations":
		return Verdict{Capability: CapPolicyAdmin}, nil
	case "permissions":
		return Verdict{Capability: CapSecurityAdmin}, nil
	default:
		if slices.Contains(gitCodeWriteResources, res) {
			return Verdict{Capability: CapCodeWrite}, nil
		}
	}
	return Verdict{Capability: CapUnclassifiedWrite}, nil
}

// buildWrite splits the build area: queueing a run is CapBuildExecute,
// everything that decides what a run EXECUTES is CapBuildAdmin. A definition
// edit is the wider power of the two — it changes what every future run does —
// which is why "execute" is not the fallback.
func buildWrite(r route) Capability {
	if r.res == "builds" {
		return CapBuildExecute
	}
	return CapBuildAdmin
}

// pipelinesWrite is the YAML-pipelines area: POST _apis/pipelines/{id}/runs
// queues a run, anything else edits a pipeline.
func pipelinesWrite(r route) Capability {
	switch r.at(3) {
	case "runs", "preview":
		return CapBuildExecute
	}
	return CapBuildAdmin
}

// releaseWrite is the classic-release area, with the same split as build.
func releaseWrite(r route) Capability {
	if r.res == "releases" || r.res == "deployments" {
		return CapBuildExecute
	}
	return CapBuildAdmin
}
