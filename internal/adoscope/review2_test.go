// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// EDGE whitespace and trailing DOTS. The service trims a segment's trailing
// spaces and dots before routing it — Windows path canonicalisation — so ".. "
// is ".." to Azure DevOps and "hooks." is "hooks". The dot-segment refusal
// compared the untrimmed text and missed both. Each spelling has its encoded
// and backslash twins.
func TestEvasionEdgeWhitespaceAndDots(t *testing.T) {
	runCases(t, []caseT{
		{name: "a trailing space after ..", req: adoReq(http.MethodPost, "/acme/_apis/wit/.. /hooks/subscriptions", `{}`), wantErr: true},
		{name: "an encoded trailing space after ..", req: adoReq(http.MethodGet, "/acme/_apis/git/..%20/tokens/pats", ""), wantErr: true},
		{name: "a leading space before ..", req: adoReq(http.MethodGet, "/acme/%20../evil/_apis/projects", ""), wantErr: true},
		{name: "a trailing space after .", req: adoReq(http.MethodGet, "/acme/. /_apis/tokens/pats", ""), wantErr: true},
		{name: "a trailing tab after ..", req: adoReq(http.MethodPost, "/acme/_apis/wit/..%09/hooks/subscriptions", `{}`), wantErr: true},
		{name: "a double-encoded trailing space", req: adoReq(http.MethodPost, "/acme/_apis/wit/..%2520/hooks/subscriptions", `{}`), wantErr: true},
		{name: "the backslash twin", req: adoReq(http.MethodPost, `/acme/_apis/wit\.. \hooks/subscriptions`, `{}`), wantErr: true},
		{name: "a trailing dot on a denied area", req: adoReq(http.MethodPost, "/acme/_apis/hooks./subscriptions", `{}`), wantErr: true},
		{name: "a trailing dot on the token area", req: adoReq(http.MethodGet, "/acme/_apis/tokens./pats", ""), wantErr: true},
		{name: "three dots", req: adoReq(http.MethodGet, "/acme/proj/.../evil/_apis/projects", ""), wantErr: true},
		{name: "a trailing space on the organisation", req: adoReq(http.MethodGet, "/acme%20/_apis/projects", ""), wantErr: true},
		{
			name:    "a $batch operation walking out through a trailing space",
			req:     adoReq(http.MethodPost, "/acme/_apis/wit/$batch", `[{"uri":"/_apis/wit/..%20/hooks/subscriptions"}]`),
			wantErr: true,
		},
		// Internal spaces and dots are ordinary name content.
		{name: "a project name with an internal space", req: adoReq(http.MethodGet, "/acme/My%20Project/_apis/git/repositories", ""), want: CapRead},
		{
			name: "a package file with internal dots",
			req: func() Request {
				r := adoReq(http.MethodGet, "/acme/_apis/packaging/feeds/f1/npm/p/-/p-1.0.0.tgz", "")
				r.Host = "pkgs.dev.azure.com"
				return r
			}(),
			want: CapRead,
		},
	})
}

// F-D — a REDUNDANT or read override on a non-POST request is ignored, not
// refused: some clients send the header on every request whether or not it
// changes anything. Raising a non-POST request to a different write is still
// refused, and POST's upward-only rule is unchanged.
func TestOverrideOnNonPostIsIgnoredUnlessItRaises(t *testing.T) {
	over := func(method, path, override, body string) Request {
		r := adoReq(method, path, body)
		r.Header = hdr("X-HTTP-Method-Override", override)
		return r
	}
	pr := "/acme/proj/_apis/git/repositories/r1/pullrequests/7"
	runCases(t, []caseT{
		{name: "GET naming GET", req: over(http.MethodGet, pr, "GET", ""), want: CapRead},
		{name: "GET naming HEAD", req: over(http.MethodGet, pr, "HEAD", ""), want: CapRead},
		{name: "HEAD naming GET", req: over(http.MethodHead, pr, "GET", ""), want: CapRead},
		{name: "PATCH naming PATCH", req: over(http.MethodPatch, pr, "PATCH", `{}`), want: CapPR},
		{name: "PATCH naming a read is classified on the PATCH", req: over(http.MethodPatch, pr, "GET", `{"completionOptions":{"bypassPolicy":true}}`), want: CapPolicyBypass},
		{name: "DELETE naming DELETE", req: over(http.MethodDelete, "/acme/proj/_apis/git/repositories/r1", "DELETE", ""), want: CapRepoAdmin},
		{name: "GET raised to PATCH is still refused", req: over(http.MethodGet, pr, "PATCH", `{}`), wantErr: true},
		{name: "PATCH raised to DELETE is still refused", req: over(http.MethodPatch, pr, "DELETE", ""), wantErr: true},
		{name: "a non-method on a GET is still refused", req: over(http.MethodGet, pr, "FROB", ""), wantErr: true},
		{name: "POST naming GET is still refused", req: over(http.MethodPost, "/acme/proj/_apis/wit/workitems/$task", "GET", `{}`), wantErr: true},
	})
}

// F-C — a read the minted token cannot perform. The read floor used to answer
// CapRead for ANY area, so an area whose read scope was missing classified as
// a read that 403s at the forge. Reads are now an enumerated table; an area
// outside it is refused as unclassified, and the three areas that were missing
// their scopes carry them.
func TestReadsAreEnumerated(t *testing.T) {
	unclassifiedRead := CapUnclassifiedRead
	onHost := func(host string, r Request) Request { r.Host = host; return r }
	runCases(t, []caseT{
		{name: "an area with no read scope a token could carry", req: adoReq(http.MethodGet, "/acme/_apis/accesscontrollists/ns1", ""), want: unclassifiedRead},
		{name: "an area this catalogue has never heard of", req: adoReq(http.MethodGet, "/acme/_apis/somethingnew/x", ""), want: unclassifiedRead},
		{name: "agent pools, which no read scope covers", req: adoReq(http.MethodGet, "/acme/_apis/distributedtask/pools", ""), want: unclassifiedRead},
		{name: "a path with no _apis at all", req: adoReq(http.MethodGet, "/acme/proj/_admin", ""), want: unclassifiedRead},
		{name: "variable groups read", req: adoReq(http.MethodGet, "/acme/proj/_apis/distributedtask/variablegroups/1", ""), want: CapRead},
		{name: "secure files read", req: adoReq(http.MethodGet, "/acme/proj/_apis/distributedtask/securefiles/1", ""), want: CapRead},
		{name: "user entitlements read", req: onHost("vsaex.dev.azure.com", adoReq(http.MethodGet, "/acme/_apis/userentitlements", "")), want: CapRead},
	})
	scopes, err := ScopesFor([]Capability{CapRead})
	if err != nil {
		t.Fatalf("ScopesFor(read) error = %v", err)
	}
	for _, s := range []string{"vso.variablegroups_read", "vso.securefiles_read", "vso.memberentitlementmanagement"} {
		if !slices.Contains(scopes, ResourceID+"/"+s) {
			t.Errorf("the read scope set lacks %q, so its area's reads 403 at the forge", s)
		}
	}
}

// F-B, F-E — the labels an approver reads must say what the capability grants.
func TestLabelsSayWhatIsGranted(t *testing.T) {
	v, err := Classify(adoReq(http.MethodPatch, "/acme/proj/_apis/release/releases/1/environments/2", `{}`))
	if err != nil || v.Capability != CapBuildExecute {
		t.Fatalf("a release deployment classified %q, %v — want build_execute", v.Capability, err)
	}
	for _, tc := range []struct {
		c     Capability
		wants []string
	}{
		{CapBuildExecute, []string{"release", "deploy", "delete"}},
		{CapBuildAdmin, []string{"release"}},
		{CapRead, []string{"service connection", "identit", "group"}},
	} {
		label := strings.ToLower(Label(tc.c))
		for _, w := range tc.wants {
			if !strings.Contains(label, w) {
				t.Errorf("Label(%q) = %q — an approver is not told it covers %q", tc.c, Label(tc.c), w)
			}
		}
	}
}

// PROFILE READS. The signed-in person's profile is an ordinary pinned read on
// the organisation's vssps host. app.vssps.visualstudio.com — the
// organisation-less host the Azure CLI's first calls used — is not a host a
// run's credential rides to (adoEntraHosts), so it has no exemption here: its
// first label is not the pinned organisation, and every request on it is
// refused like any other organisation's.
func TestOrganisationlessDiscoveryHostIsRefused(t *testing.T) {
	disc := func(method, path string) Request {
		return Request{Method: method, Host: "app.vssps.visualstudio.com", Path: path, Org: "acme"}
	}
	runCases(t, []caseT{
		{name: "the profile on the organisation-less host is refused", req: disc(http.MethodGet, "/_apis/profile/profiles/me"), wantErr: true},
		{name: "the organisation list on the organisation-less host is refused", req: disc(http.MethodGet, "/_apis/accounts"), wantErr: true},
		{name: "the token area on the organisation-less host is refused", req: disc(http.MethodGet, "/_apis/tokens/pats"), wantErr: true},

		// On an ORGANISATION host the two areas are ordinary pinned reads.
		{name: "the profile on the pinned organisation", req: func() Request {
			r := adoReq(http.MethodGet, "/acme/_apis/profile/profiles/me", "")
			r.Host = "vssps.dev.azure.com"
			return r
		}(), want: CapRead},
		{name: "another organisation's profile area is still refused", req: func() Request {
			r := adoReq(http.MethodGet, "/evil/_apis/profile/profiles/me", "")
			r.Host = "vssps.dev.azure.com"
			return r
		}(), wantErr: true},
	})
	scopes, err := ScopesFor([]Capability{CapRead})
	if err != nil {
		t.Fatalf("ScopesFor(read) error = %v", err)
	}
	if !slices.Contains(scopes, ResourceID+"/vso.profile") {
		t.Error("the read scope set lacks vso.profile, so a profile read 403s")
	}
}

// LOCATION DISCOVERY. Measured: the azure-devops CLI extension's FIRST request
// is `OPTIONS /{org}/_apis` — the Azure DevOps SDK's location-discovery call —
// and the Node SDK makes the same one. It is a pinned read with no scope, like
// connectionData. OPTIONS must never carry a write, so a body or any override
// header on it is refused, and it is admitted only on the discovery shapes.
func TestOptionsLocationDiscovery(t *testing.T) {
	opt := func(path string) Request { return adoReq(http.MethodOptions, path, "") }
	withHeader := func(r Request, kv ...string) Request { r.Header = hdr(kv...); return r }
	runCases(t, []caseT{
		{name: "the organisation's API root", req: opt("/acme/_apis"), want: CapRead},
		{name: "one area's location", req: opt("/acme/_apis/git"), want: CapRead},
		{name: "one area's location under a project", req: opt("/acme/proj/_apis/wit"), want: CapRead},
		{name: "an area this catalogue has no read entry for", req: opt("/acme/_apis/distributedtask"), want: CapRead},
		{name: "a legacy host's API root", req: func() Request { r := opt("/_apis"); r.Host = "acme.visualstudio.com"; return r }(), want: CapRead},

		{name: "another organisation is refused", req: opt("/evil/_apis"), wantErr: true},
		{name: "a body on OPTIONS is refused", req: adoReq(http.MethodOptions, "/acme/_apis", `{"x":1}`), wantErr: true},
		{name: "a declared body the peek did not carry is refused", req: withHeader(opt("/acme/_apis"), "Content-Length", "5"), wantErr: true},
		{name: "a chunked body is refused", req: withHeader(opt("/acme/_apis"), "Transfer-Encoding", "chunked"), wantErr: true},
		{name: "an override naming a read is refused on OPTIONS", req: withHeader(opt("/acme/_apis"), "X-HTTP-Method-Override", "GET"), wantErr: true},
		{name: "an override naming OPTIONS itself is refused", req: withHeader(opt("/acme/_apis"), "X-HTTP-Method-Override", "OPTIONS"), wantErr: true},
		{name: "an override naming a write is refused", req: withHeader(opt("/acme/_apis/git"), "X-HTTP-Method-Override", "DELETE"), wantErr: true},
		{name: "a denied area stays denied", req: opt("/acme/_apis/tokens"), want: CapDeniedTokens},
		{name: "OPTIONS below an area is not location discovery", req: opt("/acme/proj/_apis/git/repositories"), want: CapUnclassifiedRead},
		{name: "OPTIONS off _apis is not location discovery", req: opt("/acme/proj/_admin"), want: CapUnclassifiedRead},
		{name: "a zero Content-Length is not a body", req: withHeader(opt("/acme/_apis"), "Content-Length", "0"), want: CapRead},
	})
}
