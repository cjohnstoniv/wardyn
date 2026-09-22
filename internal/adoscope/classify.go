// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"
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
	// BodyWithheld marks a request whose body the caller has not read. A
	// route whose classification reads the body then fails with ErrNeedsBody
	// rather than classifying, and the caller peeks and asks again. Every
	// other route classifies on its path alone, so a gate can stream a package
	// publish or a wiki attachment through untouched — and the list of routes
	// that need the body is the one Classify walks, not a second copy.
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
	if method == http.MethodOptions {
		if err := optionsCarriesNoBody(req); err != nil {
			return Verdict{}, err
		}
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

// locationDiscovery classifies an OPTIONS request.
//
// OPTIONS is the Azure DevOps SDK's API location-discovery call — MEASURED as
// the azure-devops CLI extension's first request, `OPTIONS /{org}/_apis`, and
// the Node SDK makes the same one. It returns route templates, not data, and
// like connectionData it needs no scope. It is admitted on exactly the
// discovery shapes — the API root, or one area's location, at the
// organisation or project level — and is otherwise an unclassified read.
//
// Any area name is accepted here, including ones readAreas does not list: the
// SDK asks for the location of whatever area it is about to call, and the
// denied areas were already refused before this is reached. What makes it
// safe to be that open is that OPTIONS cannot carry a write: a body and every
// override header are refused before routing.
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

// readAreas is EVERY area this catalogue answers CapRead for, with the scope a
// token must carry to perform that read. An empty scope is an area the
// service does not scope-gate at all (discovery, which every client calls
// first). A key "area/resource" is a resource whose read scope differs from
// its area's; the area alone is looked up only when no such key exists.
//
// It is a CLOSED table and the read scope set is DERIVED from it (see
// readScopes), which is what keeps the two from drifting: the read floor used
// to answer CapRead for any area at all, so an area whose read scope was
// missing — variable groups, secure files, entitlements — classified as a read
// the minted token could not perform. A read outside this table is
// CapUnclassifiedRead, which is not grantable: an area no read scope covers
// (ACLs, agent pools) is refused here instead of 403ing at the forge.
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
	// A package client's feed route carries no _apis segment, so it names no
	// area; it is the packaging area all the same.
	if packagePublish(r) {
		return readAreas["packaging"], true
	}
	if s, ok := readAreas[r.area+"/"+r.res]; ok {
		return s, true
	}
	s, ok := readAreas[r.area]
	return s, ok
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
// POST only, it must name a write, and anything else on a POST is refused, not
// ignored. Off a POST, see the case below.
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
		// OPTIONS is admitted only as location discovery, which is a read
		// precisely because nothing can ride on it. An override header — even
		// a redundant one — is the one way a write could, so it is refused
		// outright rather than weighed.
		return "", fmt.Errorf("adoscope: X-HTTP-Method-Override on an OPTIONS request — discovery takes no override")
	case !knownMethod(over):
		return "", fmt.Errorf("adoscope: X-HTTP-Method-Override: %q is not an HTTP method", ov[0])
	case base != http.MethodPost:
		// Off a POST the header is undocumented. Some clients send it on every
		// request whether or not it changes anything, so an override naming
		// the method already on the line, or a read, is IGNORED — classifying
		// on the base can only be the same answer or a higher one. An override
		// that would turn this request into a DIFFERENT write is the one thing
		// that could matter, and it is refused.
		if over == base || slices.Contains(readMethods, over) {
			return base, nil
		}
		return "", fmt.Errorf("adoscope: X-HTTP-Method-Override: %q on a %s — the header raises only a POST", ov[0], base)
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
//
// "Segment" means what AZURE DEVOPS means by it, which is split on "\\" as
// well as "/" — see isPathSeparator. Splitting on "/" alone left a backslash
// inside what this package treated as one segment, so "..\\" walked through it
// past every rule above; the dot-segment refusal held for exactly one of the
// two spellings the service routes.
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

// isPathSeparator reports whether c ends a path segment to Azure DevOps.
//
// BACKSLASH IS ONE, and this is the whole reason the function exists rather
// than a literal '/'. The service routes "\\" exactly as it routes "/" —
// live-confirmed: "..\\" walks through it into a denied area and out of the
// pinned organisation — so a classifier that splits on "/" alone is reading a
// different path from the one the service will serve. Both the split and the
// decoded-byte refusal ask this, so the two can never disagree about which
// bytes are separators.
func isPathSeparator(c rune) bool { return c == '/' || c == '\\' }

// decodeSegments splits rawPath into decoded, lowercased, non-empty segments,
// refusing the shapes parseRoute documents.
func decodeSegments(rawPath string) ([]string, error) {
	var out []string
	for _, raw := range strings.FieldsFunc(rawPath, isPathSeparator) {
		seg, err := unescapeSegment(raw)
		if err != nil {
			return nil, err
		}
		if hazard := segmentHazard(seg); hazard != "" {
			return nil, fmt.Errorf("adoscope: path segment %q %s", raw, hazard)
		}
		if hidesStructure(seg) {
			return nil, fmt.Errorf("adoscope: path segment %q is encoded more than once around structure — a layer that decodes again would route it differently", raw)
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
		if isPathSeparator(rune(c)) {
			return "", fmt.Errorf("adoscope: path segment %q decodes to a second segment", raw)
		}
		b.WriteByte(c)
		i += 2
	}
	return strings.ToLower(b.String()), nil
}

// segmentHazard names why a decoded segment would not be routed the way it
// reads, or "" when it would.
//
// Every case is a spelling the SERVICE normalises before it routes, so the
// text here and the route there disagree:
//   - "." and ".." are resolved outright;
//   - leading or trailing whitespace, and a trailing dot, are trimmed first —
//     Windows path canonicalisation — so ".. " is "..", "..." is "..", and
//     "hooks." is the denied "hooks" area. Refusing the edge characters is the
//     fail-closed reading, and no name on this API legitimately ends in one;
//   - a separator still inside the segment. While the split in decodeSegments
//     is correct nothing reaches this case: the split removed every raw
//     separator and unescapeSegment refuses every decoded one. It is a SECOND,
//     INDEPENDENT LINE — with the split reverted to "/" alone and this kept,
//     every separator evasion is still refused, and the only cost is that a
//     legitimate backslash-delimited path is refused too.
func segmentHazard(seg string) string {
	switch {
	case seg == "." || seg == "..":
		return "is a dot segment — the service resolves it to a different route"
	case strings.TrimFunc(seg, unicode.IsSpace) != seg:
		return "has leading or trailing whitespace — the service trims it before routing"
	case strings.HasSuffix(seg, "."):
		return "ends in a dot — the service trims it before routing"
	case strings.ContainsFunc(seg, isPathSeparator):
		return "still holds a separator"
	}
	return ""
}

// maxDecodeDepth bounds hidesStructure. Nothing legitimate on this API is
// percent-encoded more than once, so a segment still changing after this many
// further rounds is refused rather than followed.
const maxDecodeDepth = 4

// hidesStructure reports whether decoding seg AGAIN — as any layer between here
// and the service that decodes once more would — yields any segmentHazard at
// any depth.
//
// It exists because "%252F" decodes once to the literal text "%2F": harmless
// to a service that decodes once, and a separator to anything that decodes
// twice. Whether such a layer sits in the path is not knowable from here, so
// the answer that cannot be wrong is to refuse the segment.
//
// The re-decode is LENIENT on purpose — valid escapes decoded, malformed ones
// kept as literal text — because that is the most dangerous decoder a request
// could meet: a strict one refuses "%2F%zz" outright, a lenient one decodes it
// to "/%zz". Assuming the lenient one is the fail-closed reading, and it costs
// no legitimate name anything: "100%" and "50%off" reach a fixpoint unchanged.
func hidesStructure(seg string) bool {
	for range maxDecodeDepth {
		next := percentDecodeLenient(seg)
		if next == seg {
			return false
		}
		if segmentHazard(next) != "" {
			return true
		}
		seg = next
	}
	return true
}

// percentDecodeLenient decodes every valid %XY in s once and keeps every
// malformed one as literal text. See hidesStructure for why lenient.
func percentDecodeLenient(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if hi, lo := unhex(s[i+1]), unhex(s[i+2]); hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
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

// packagePublish reports whether r is a package client's own feed route on the
// packages host: [{project}/]_packaging/{feed}/{protocol}/…, with no _apis
// segment. Positional, like every other rule here: _packaging must be the first
// segment after the organisation, or the second when a project precedes it.
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
// read out of the body: an annotated tag, or a fork sync onto a branch. Every
// REST ref move is held to policy_bypass (adoRefProtected, ado_gate.go), so
// these are too.
var gitRefMoveResources = []string{"annotatedtags", "forksyncrequests"}

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
	case "", "importrequests":
		// An import request replaces the repository's content wholesale.
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
