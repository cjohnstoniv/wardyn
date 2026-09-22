// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// EDGE WHITESPACE AND TRAILING DOTS. The service trims a segment's trailing
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
