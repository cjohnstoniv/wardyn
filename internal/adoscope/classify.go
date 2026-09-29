// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Request is ONE candidate Azure DevOps request, as much of it as a classifier
// can be given without buffering the whole body. A struct rather than a
// parameter list because Org and RefProtected are properties of the row and
// run, not of the request, and neither can be guessed from the bytes.
type Request struct {
	// Method is the HTTP method as it arrived on the request line. An
	// override header may RAISE it — see Classify.
	Method string
	// Host is the request's host WITHOUT a port.
	Host string
	// Path is the request's path, still percent-encoded and WITHOUT the query string.
	Path string
	// Header may be nil. Keys are compared case-insensitively, so
	// wire-spelling vs. canonicalised map keys read the same way.
	Header http.Header
	// BodyPeek is the first MaxBodyPeek bytes of the body, or nil. A route
	// that needs the body and cannot see all of it is REFUSED, never
	// classified on the visible prefix.
	BodyPeek []byte
	// Org is the organisation the provider row pinned. REQUIRED: an unpinned
	// request is refused.
	Org string
	// RefProtected answers whether one ref is covered by a branch policy,
	// given the full ref name as the request body spelled it. Optional — when
	// nil, a ref move classifies as CapCodeWrite and the caller gets the ref
	// names in Verdict.Refs to re-decide against its own cache.
	RefProtected func(ref string) bool
	// BodyWithheld marks a request whose body the caller has not read. A
	// route classified by the body fails with ErrNeedsBody instead, and the
	// caller peeks and asks again.
	BodyWithheld bool
}

// ErrNeedsBody is Classify's answer for a BodyWithheld request on a route it
// classifies by the body: peek the body, clear BodyWithheld and ask again.
var ErrNeedsBody = errors.New("adoscope: this route is classified on its body")

// Verdict is one classification.
type Verdict struct {
	// Capability is the ONE access this request needs.
	Capability Capability
	// Refs are the ref names the request's body named, if any. Always
	// returned for a ref move, whether or not RefProtected was supplied.
	Refs []string
}

// readMethods are the methods that cannot change anything.
var readMethods = []string{http.MethodGet, http.MethodHead, http.MethodOptions}

// writeMethods are the methods that can.
var writeMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// Classify names the ONE capability req needs, or refuses req. It refuses
// only where no capability could be honest: a non-Azure-DevOps host, a
// mismatched organisation, an obscured path, an undocumented method
// override, a git-over-HTTP endpoint, or a body the peek cannot see whole.
// Everything else classifies, and an unrecognized write classifies as the
// non-grantable CapUnclassifiedWrite.
//
// GIT-OVER-HTTP IS OUT OF SCOPE and refused by name: the ref names a push
// carries live in a pack protocol this catalogue does not parse, so no
// answer here could tell a clone from a push onto a protected branch.
func Classify(req Request) (Verdict, error) {
	method, err := effectiveMethod(req.Method, req.Header)
	if err != nil {
		return Verdict{}, err
	}
	if method == http.MethodOptions {
		if err := optionsCarriesNoBody(req); err != nil {
			return Verdict{}, err
		}
	}
	r, err := parseRoute(req.Host, req.Path, req.Org)
	if err != nil {
		return Verdict{}, err
	}
	// Denied areas are checked BEFORE the method: a GET of the token area is
	// a read of exactly the thing no lane may see.
	if c, ok := deniedAreas[r.area]; ok {
		return Verdict{Capability: c}, nil
	}
	if method == http.MethodOptions {
		return locationDiscovery(r), nil
	}
	if slices.Contains(readMethods, method) || readWrite(r) {
		if _, ok := readScope(r); ok {
			return Verdict{Capability: CapRead}, nil
		}
		return Verdict{Capability: CapUnclassifiedRead}, nil
	}
	return classifyWrite(method, r, req)
}

// locationDiscovery classifies an OPTIONS request: the SDKs' API
// location-discovery call. Returns route templates, not data, and needs no
// scope. Admitted only on discovery shapes (API root, or one area's location
// at org/project level); otherwise an unclassified read. Any area name is
// accepted, including ones readAreas doesn't list, because OPTIONS can't
// carry a write — a body and every override header are refused before routing.
func locationDiscovery(r route) Verdict {
	if (r.apis == 0 || r.apis == 1) && r.at(2) == "" {
		return Verdict{Capability: CapRead}
	}
	return Verdict{Capability: CapUnclassifiedRead}
}

// optionsCarriesNoBody refuses an OPTIONS request that carries, or declares,
// a body. Discovery sends none, and a body is the only place a write could
// hide on a method the service is otherwise told is a read.
func optionsCarriesNoBody(req Request) error {
	if req.BodyWithheld {
		return ErrNeedsBody
	}
	if len(req.BodyPeek) > 0 {
		return fmt.Errorf("adoscope: an OPTIONS request carries a body — discovery sends none")
	}
	n, known, err := declaredLength(req.Header)
	if err != nil {
		return err
	}
	if known && n > 0 {
		return fmt.Errorf("adoscope: an OPTIONS request declares a %d-byte body — discovery sends none", n)
	}
	if len(headerValues(req.Header, "Transfer-Encoding")) > 0 {
		return fmt.Errorf("adoscope: an OPTIONS request declares a streamed body — discovery sends none")
	}
	return nil
}

// readAreas is EVERY area this catalogue answers CapRead for, with the scope
// a token must carry to perform that read. An empty scope is an area the
// service doesn't scope-gate at all. A key "area/resource" is a resource
// whose read scope differs from its area's. CLOSED table: a read outside it
// is CapUnclassifiedRead (not grantable) rather than 403ing at the forge.
var readAreas = map[string]string{
	"git": "vso.code", "policy": "vso.code", "search": "vso.code",
	"wit": "vso.work", "work": "vso.work",
	"build": "vso.build", "pipelines": "vso.build",
	"release":   "vso.release",
	"wiki":      "vso.wiki",
	"packaging": "vso.packaging", "packages": "vso.packaging",
	"projects": "vso.project", "projectcollections": "vso.project",
	"serviceendpoint": "vso.serviceendpoint",
	"graph":           "vso.graph",
	"identities":      "vso.identity",
	"test":            "vso.test", "testplan": "vso.test", "testresults": "vso.test",
	"analytics":        "vso.analytics",
	"userentitlements": "vso.memberentitlementmanagement", "groupentitlements": "vso.memberentitlementmanagement",
	"memberentitlements":             "vso.memberentitlementmanagement",
	"distributedtask/variablegroups": "vso.variablegroups_read",
	"distributedtask/securefiles":    "vso.securefiles_read",
	"connectiondata":                 "", "resourceareas": "",
	// The signed-in person's profile and organisation list.
	"profile": "vso.profile", "accounts": "vso.profile",
}

// readScope is the scope a read of r needs, and whether r is a read this
// catalogue knows at all.
func readScope(r route) (string, bool) {
	// A package client's feed route carries no _apis segment, but it is the
	// packaging area all the same.
	if packagePublish(r) {
		return readAreas["packaging"], true
	}
	if s, ok := readAreas[r.area+"/"+r.res]; ok {
		return s, true
	}
	s, ok := readAreas[r.area]
	return s, ok
}

// effectiveMethod is the method the SERVER will act on, not always the one on
// the request line: Azure DevOps honours X-HTTP-Method-Override, so a POST
// carrying `X-HTTP-Method-Override: PATCH` is a PATCH.
//
// AN OVERRIDE MAY ONLY RAISE: on a POST it must name a write, refused rather
// than ignored otherwise — a POSTed push must not classify as a READ on the
// word of a header the caller controls.
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
	}
	over := strings.ToUpper(strings.TrimSpace(ov[0]))
	switch {
	case base == http.MethodOptions:
		// OPTIONS is a read precisely because nothing can ride on it, so any
		// override is refused outright.
		return "", fmt.Errorf("adoscope: X-HTTP-Method-Override on an OPTIONS request — discovery takes no override")
	case !knownMethod(over):
		return "", fmt.Errorf("adoscope: X-HTTP-Method-Override: %q is not an HTTP method", ov[0])
	case base != http.MethodPost:
		// Off a POST the header is undocumented: an override naming the
		// method already on the line, or a read, is IGNORED; one that would
		// turn this into a DIFFERENT write is refused.
		if over == base || slices.Contains(readMethods, over) {
			return base, nil
		}
		return "", fmt.Errorf("adoscope: X-HTTP-Method-Override: %q on a %s — the header raises only a POST", ov[0], base)
	case !slices.Contains(writeMethods, over):
		return "", fmt.Errorf("adoscope: X-HTTP-Method-Override: %q would lower a POST to a read — an override may only raise", ov[0])
	}
	return over, nil
}

// headerValues is h's values for name, matched CASE-INSENSITIVELY — Header.Values
// canonicalises the key given but not keys already in the map, hiding a non-canonical spelling.
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
// about. TRACE and CONNECT are deliberately absent: neither is an Azure DevOps API call.
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
// resource, and so on — or "". EVERY route rule indexes through this rather
// than searching segments: a repository named "items" by its creator must
// not make its deletion read as a code write.
func (r route) at(n int) string {
	if r.apis < 0 {
		return ""
	}
	if i := r.apis + n; i >= 0 && i < len(r.segs) {
		return r.segs[i]
	}
	return ""
}

// parseRoute decodes path, pins it to org, and splits it at _apis. Decoding
// refuses rather than resolves three things: percent-encoding that hides
// structure, a segment decoding to contain a separator, and a "." or ".."
// segment (Azure DevOps resolves these server-side; refusing is simpler and
// safer than RFC 3986 resolution). "Segment" also splits on "\\" (isPathSeparator).
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
// tokens: dev.azure.com and its service subdomains, or a legacy
// <org>.visualstudio.com. An Azure DevOps Server host is absent on purpose —
// it doesn't accept Entra tokens at all.
func azureDevOpsHost(h string) bool {
	return h == "dev.azure.com" || strings.HasSuffix(h, ".dev.azure.com") ||
		strings.HasSuffix(h, ".visualstudio.com")
}

// isPathSeparator reports whether c ends a path segment to Azure DevOps.
// BACKSLASH IS ONE: the service routes "\\" exactly as it routes "/", so a
// classifier that splits on "/" alone reads a different path from the one
// the service will serve.
func isPathSeparator(c rune) bool { return c == '/' || c == '\\' }

// decodeSegments splits rawPath into decoded, lowercased, non-empty segments,
// refusing the shapes parseRoute documents. Each segment is decoded by
// UnescapeName (names.go), shared by every door that reads an Azure DevOps name.
func decodeSegments(rawPath string) ([]string, error) {
	var out []string
	for _, raw := range strings.FieldsFunc(rawPath, isPathSeparator) {
		seg, err := UnescapeName(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, strings.ToLower(seg))
	}
	return out, nil
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

// deniedAreas is the area -> refusal table, refused for EVERY method,
// including reads. SECURITY: the token family is deliberately WIDE — a lane
// that can obtain a second credential (session-token, delegated-auth,
// web-platform-auth, not just the documented PAT door) is a lane that can leave the lane.
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

// readWrite reports whether r is one of the POSTs that only READ. Azure
// DevOps uses POST for queries whose input is too big for a query string;
// treating method as intent would charge a work-item query the same
// capability as an edit. Closed, short, and POSITIONAL: every entry is a
// documented query endpoint matched where the route puts it.
func readWrite(r route) bool {
	switch r.area {
	case "wit":
		return r.res == "wiql" || r.res == "workitemsbatch"
	case "git":
		// {project}/_apis/git/repositories/{repositoryId}/pullrequestquery
		return r.res == "repositories" && r.at(4) == "pullrequestquery"
	case "search":
		// Code/work-item search lives on its own host; "search" under
		// dev.azure.com is not a documented query door.
		return strings.HasPrefix(r.host, "almsearch.")
	}
	return false
}

// classifyWrite names the capability a write needs, by area. DEFAULT is
// CapUnclassifiedWrite: a new area is refused until someone adds it here,
// rather than inheriting whatever the nearest area was granted.
func classifyWrite(method string, r route, req Request) (Verdict, error) {
	if packagePublish(r) {
		return Verdict{Capability: CapPackagingWrite}, nil
	}
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

// packageProtocols are the feed protocols a package client publishes through.
var packageProtocols = []string{"npm", "nuget", "pypi", "maven", "upack"}

// packagePublish reports whether r is a package client's own feed route on
// the packages host: [{project}/]_packaging/{feed}/{protocol}/…, with no
// _apis segment.
func packagePublish(r route) bool {
	if r.apis >= 0 || (r.host != "pkgs.dev.azure.com" && !strings.HasSuffix(r.host, ".pkgs.visualstudio.com")) {
		return false
	}
	i := slices.Index(r.segs, "_packaging")
	return (i == 0 || i == 1) && i+2 < len(r.segs) && slices.Contains(packageProtocols, r.segs[i+2])
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
	"items", "commits", "merges", "cherrypicks", "reverts", "suggestions",
}

// gitRefMoveResources create or move a ref whose name this catalogue does not
// read out of the body; held to policy_bypass like every other REST ref move.
var gitRefMoveResources = []string{"annotatedtags", "forksyncrequests"}

// gitWrite is the git area, carrying four capabilities behind one scope: the
// repository object (CapRepoAdmin), its policies (CapPolicyAdmin), its pull
// requests (CapPR), and its refs (CapCodeWrite or CapPolicyBypass, decided by
// the body). POSITIONAL split, since one segment is a caller-chosen repository name.
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
// With no {resource} the request is about the repository OBJECT itself.
func gitRepositoryWrite(method string, r route, req Request) (Verdict, error) {
	switch res := r.at(4); res {
	case "", "importrequests":
		// An import request replaces the repository's content wholesale.
		return Verdict{Capability: CapRepoAdmin}, nil
	case "refs":
		// PATCH on refs is Update Ref, naming its ref in the ?filter= query
		// this catalogue never sees, so the run-ref rule cannot apply here.
		// Only POST (Update Refs, refs named in the body) is read ref by ref.
		if method != http.MethodPost {
			return Verdict{Capability: CapPolicyBypass}, nil
		}
		return refWrite(req)
	case "pushes":
		return refWrite(req)
	case "pullrequests":
		return pullRequestWrite(method, req)
	case "policyconfigurations":
		return Verdict{Capability: CapPolicyAdmin}, nil
	case "permissions":
		return Verdict{Capability: CapSecurityAdmin}, nil
	default:
		if slices.Contains(gitRefMoveResources, res) {
			return Verdict{Capability: CapPolicyBypass}, nil
		}
		if slices.Contains(gitCodeWriteResources, res) {
			return Verdict{Capability: CapCodeWrite}, nil
		}
	}
	return Verdict{Capability: CapUnclassifiedWrite}, nil
}

// buildWrite splits the build area: queueing a run is CapBuildExecute,
// everything that decides what a run EXECUTES is CapBuildAdmin (the wider
// power, so it is the fallback, not "execute").
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
