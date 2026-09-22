// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"fmt"
	"net/http"
	"net/url"
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
	// Method is the HTTP method as it arrived. An override header, if
	// present, wins — see Classify.
	Method string
	// Host is the request's host, without a port.
	Host string
	// Path is the request's path, still percent-encoded.
	Path string
	// Header is the request's headers. May be nil.
	Header http.Header
	// BodyPeek is the first MaxBodyPeek bytes of the body, or nil when there
	// is none. A route that needs the body and cannot see all of it is
	// REFUSED, never classified on the visible prefix.
	BodyPeek []byte
	// Org is the organisation the provider row pinned. REQUIRED: an
	// unpinned request is refused, because "some Azure DevOps organisation"
	// is not a policy.
	Org string
	// RefProtected answers whether one ref name (refs/heads/main) is covered
	// by a branch policy. Optional — when nil, a ref move classifies as
	// CapCodeWrite and the caller is handed the ref names in Verdict.Refs so
	// it can re-decide against its own per-run cache.
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
// It refuses — returns an error — only where no capability could be honest:
// a host that is not Azure DevOps, an organisation that is not the row's, a
// path whose structure is hidden behind percent-encoding, a method override
// naming something that is not a method, or a body the peek cannot see whole.
// Everything else classifies, and a write it does not recognize classifies as
// CapUnclassifiedWrite, which is not grantable.
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
	if c, ok := deniedArea(r.area); ok {
		return Verdict{Capability: c}, nil
	}
	if slices.Contains(readMethods, method) {
		return Verdict{Capability: CapRead}, nil
	}
	if readWrite(r) {
		return Verdict{Capability: CapRead}, nil
	}
	return classifyWrite(method, r, req)
}

// effectiveMethod is the method the SERVER will act on, which is not always
// the one on the request line: Azure DevOps honours X-HTTP-Method-Override, so
// a GET carrying `X-HTTP-Method-Override: PATCH` is a PATCH. Classifying the
// request line would hand a read's capability to a write.
//
// A header naming something that is not a method, or repeated, is refused
// rather than ignored — ignoring it means guessing which of the two the server
// will pick.
func effectiveMethod(method string, h http.Header) (string, error) {
	base := strings.ToUpper(strings.TrimSpace(method))
	if !knownMethod(base) {
		return "", fmt.Errorf("adoscope: %q is not an HTTP method this catalogue classifies", method)
	}
	if h == nil {
		return base, nil
	}
	ov := h.Values("X-HTTP-Method-Override")
	switch {
	case len(ov) == 0:
		return base, nil
	case len(ov) > 1:
		return "", fmt.Errorf("adoscope: %d X-HTTP-Method-Override values — which one the server acts on is not knowable", len(ov))
	}
	over := strings.ToUpper(strings.TrimSpace(ov[0]))
	if !knownMethod(over) {
		return "", fmt.Errorf("adoscope: X-HTTP-Method-Override: %q is not an HTTP method", ov[0])
	}
	return over, nil
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
	// area is the segment after _apis ("git", "wit", …), or "".
	area string
	// res is the segment after the area, or "".
	res string
	// tail is the segments after res.
	tail []string
}

// parseRoute decodes path, pins it to org, and splits it at _apis.
//
// The DECODING is the security-relevant half. Matching the raw path would let
// "%5Fapis/tokens" slip past the denied-area check and land in the read floor,
// so every segment is percent-decoded before it is matched. A segment that
// decodes to something containing a separator is REFUSED rather than split:
// "a%2Fb" is one segment to the router and two to the reader, and a classifier
// that picks either answer is wrong for the other.
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

// decodeSegments splits rawPath into decoded, lowercased, non-empty segments.
func decodeSegments(rawPath string) ([]string, error) {
	var out []string
	for _, raw := range strings.Split(rawPath, "/") {
		if raw == "" {
			continue
		}
		seg, err := url.PathUnescape(raw)
		if err != nil {
			return nil, fmt.Errorf("adoscope: path segment %q is not decodable", raw)
		}
		if strings.ContainsAny(seg, "/\\") {
			return nil, fmt.Errorf("adoscope: path segment %q decodes to a second segment", raw)
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
// it. A path with no _apis (a git-over-HTTP endpoint, or the web UI) yields an
// empty area, which reads as the fail-closed answer for a write.
func splitAtAPIs(host string, segs []string) route {
	r := route{host: host, segs: segs}
	i := slices.Index(segs, "_apis")
	if i < 0 {
		return r
	}
	if i+1 < len(segs) {
		r.area = segs[i+1]
	}
	if i+2 < len(segs) {
		r.res = segs[i+2]
	}
	if i+3 < len(segs) {
		r.tail = segs[i+3:]
	}
	return r
}

// has reports whether the segments after the area include seg — the shape the
// pull-request and ref routes need, where the interesting word sits at a depth
// that varies with whether the URL named a project.
func (r route) has(seg string) bool {
	return r.res == seg || slices.Contains(r.tail, seg)
}

// deniedAreas is the area -> refusal table. These are refused for EVERY
// method, including reads.
var deniedAreas = map[string]Capability{
	"tokens":              CapDeniedTokens,
	"tokenadmin":          CapDeniedTokens,
	"tokenadministration": CapDeniedTokens,
	"hooks":               CapDeniedServiceHooks,
	"servicehooks":        CapDeniedServiceHooks,
	"extensionmanagement": CapDeniedExtensions,
	"gallery":             CapDeniedExtensions,
	"contribution":        CapDeniedInternal,
}

// deniedArea looks area up in the refusal table.
func deniedArea(area string) (Capability, bool) {
	c, ok := deniedAreas[area]
	return c, ok
}

// readWrite reports whether r is one of the POSTs that only READ. Azure DevOps
// uses POST for queries whose input is too big for a query string, so treating
// method as intent would charge a work-item query the same capability as a
// work-item edit — and an operator who granted "read" would watch reads fail.
//
// The list is closed and short by design: every entry is a documented query
// endpoint with no write side. _apis/Contribution/HierarchyQuery is NOT here —
// it is a query too, but one that reads every area at once, and it is refused
// as a denied area before this is reached.
func readWrite(r route) bool {
	switch r.area {
	case "wit":
		return r.res == "wiql" || r.res == "workitemsbatch"
	case "git":
		return r.has("pullrequestquery")
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
func gitWrite(method string, r route, req Request) (Verdict, error) {
	switch {
	case r.has("refs") || r.has("pushes"):
		return refWrite(req)
	case r.has("pullrequests") || r.has("pullrequest") || r.res == "pullrequests":
		return pullRequestWrite(method, req)
	case r.has("policyconfigurations") || r.has("policy"):
		return Verdict{Capability: CapPolicyAdmin}, nil
	case r.has("permissions"):
		return Verdict{Capability: CapSecurityAdmin}, nil
	case slices.ContainsFunc(gitCodeWriteResources, r.has):
		return Verdict{Capability: CapCodeWrite}, nil
	case r.res == "repositories":
		// repositories/{id} with nothing after it — creating, renaming or
		// deleting the repository object.
		return Verdict{Capability: CapRepoAdmin}, nil
	}
	// A git-over-HTTP write (_git/…/git-receive-pack) lands here, with no
	// _apis area at all, and stays unclassified ON PURPOSE: the ref names a
	// push carries are in a pack protocol this catalogue does not parse, so
	// calling it CapCodeWrite would grant a push to a protected branch that
	// the REST ref route above refuses.
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

// pipelinesWrite is the YAML-pipelines area: POST …/pipelines/{id}/runs queues
// a run, anything else edits a pipeline.
func pipelinesWrite(r route) Capability {
	if r.has("runs") || r.has("preview") {
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
