// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"net/http"
	"testing"
)

// F1 — DOT SEGMENTS. Azure DevOps resolves "." and ".." server-side (verified
// live: /x/../_apis/resourceareas answers 200 where /x/_apis/resourceareas
// 404s), so a classifier that reads the segments as written is reading a
// different URL from the one the service will route. Every case here reached a
// GRANTABLE capability, or the read floor, on a route the row never permitted.
func TestEvasionDotSegments(t *testing.T) {
	runCases(t, []caseT{
		{
			name:    "a denied area reached through .. under a grantable capability",
			req:     adoReq(http.MethodPost, "/acme/_apis/wit/../hooks/subscriptions", `{}`),
			wantErr: true,
		},
		{
			name:    "the token area reached through .. under the read floor",
			req:     adoReq(http.MethodGet, "/acme/_apis/git/../tokens/pats", ""),
			wantErr: true,
		},
		{
			name:    "the organisation pin walked off with ..",
			req:     adoReq(http.MethodGet, "/acme/../evil/_apis/projects", ""),
			wantErr: true,
		},
		{
			name:    "a push to ANOTHER organisation",
			req:     adoReq(http.MethodPost, "/acme/../evil/proj/_apis/git/repositories/r/pushes", `{"refUpdates":[{"name":"refs/heads/main"}]}`),
			wantErr: true,
		},
		{
			name:    "percent-encoded dots",
			req:     adoReq(http.MethodGet, "/acme/%2e%2e/evil/_apis/projects", ""),
			wantErr: true,
		},
		{
			name:    "percent-encoded dots in upper case",
			req:     adoReq(http.MethodGet, "/acme/%2E%2E/evil/_apis/projects", ""),
			wantErr: true,
		},
		{
			name:    "a single dot segment",
			req:     adoReq(http.MethodGet, "/acme/./_apis/tokens/pats", ""),
			wantErr: true,
		},
		{
			name:    "a trailing dot segment",
			req:     adoReq(http.MethodPost, "/acme/_apis/wit/workitems/./../../hooks/subscriptions", `{}`),
			wantErr: true,
		},
		{
			name:    "a $batch operation that walks out of the work-item area",
			req:     adoReq(http.MethodPost, "/acme/_apis/wit/$batch", `[{"uri":"/_apis/wit/../hooks/subscriptions"}]`),
			wantErr: true,
		},
	})
}

// F-A — BACKSLASH IS A SEPARATOR. Azure DevOps routes "\" exactly as it routes
// "/", so every dot-segment evasion above has a backslash twin that the
// forward-slash refusal alone did not see: the path split on "/" only, a "\"
// stayed INSIDE what the classifier treated as one segment, and "..\" walked
// through it into a denied area or out of the pinned organisation. Each case is
// the backslash twin of a forward-slash case in TestEvasionDotSegments.
func TestEvasionBackslashSeparator(t *testing.T) {
	runCases(t, []caseT{
		{
			name:    "a denied area reached through ..\\ under a grantable capability",
			req:     adoReq(http.MethodPost, `/acme/_apis/wit/..\hooks/subscriptions`, `{}`),
			wantErr: true,
		},
		{
			name:    "the token area reached through ..\\ under the read floor",
			req:     adoReq(http.MethodGet, `/acme/_apis/git\..\tokens/pats`, ""),
			wantErr: true,
		},
		{
			name:    "the organisation pin walked off with ..\\",
			req:     adoReq(http.MethodGet, `/acme/proj\..\..\evil/_apis/projects`, ""),
			wantErr: true,
		},
		{
			name:    "a push to ANOTHER organisation through backslashes",
			req:     adoReq(http.MethodPost, `/acme/proj\..\..\evil/_apis/git/repositories/r/pushes`, `{"refUpdates":[{"name":"refs/heads/main"}]}`),
			wantErr: true,
		},
		{
			name:    "an organisation segment that carries its own escape",
			req:     adoReq(http.MethodGet, `/acme\..\evil/_apis/projects`, ""),
			wantErr: true,
		},
		{
			name:    "a percent-encoded backslash",
			req:     adoReq(http.MethodGet, "/acme/proj%5c..%5c..%5cevil/_apis/projects", ""),
			wantErr: true,
		},
		{
			name:    "a percent-encoded backslash in upper case",
			req:     adoReq(http.MethodGet, "/acme/proj%5C..%5C..%5Cevil/_apis/projects", ""),
			wantErr: true,
		},
		{
			name:    "a single dot between backslashes",
			req:     adoReq(http.MethodGet, `/acme/.\_apis/tokens/pats`, ""),
			wantErr: true,
		},
		{
			name:    "the two separators mixed",
			req:     adoReq(http.MethodPost, `/acme/_apis/wit/workitems\./..\../hooks/subscriptions`, `{}`),
			wantErr: true,
		},
		{
			name:    "a $batch operation that walks out through a backslash",
			req:     adoReq(http.MethodPost, "/acme/_apis/wit/$batch", `[{"uri":"/_apis/wit/..\\hooks/subscriptions"}]`),
			wantErr: true,
		},
		{
			name:    "a $batch operation with an encoded backslash",
			req:     adoReq(http.MethodPost, "/acme/_apis/wit/$batch", `[{"uri":"/_apis/wit/..%5Chooks/subscriptions"}]`),
			wantErr: true,
		},

		// The model is "a backslash is a separator", NOT "a backslash is
		// refused": a backslash-delimited path that stays inside its own
		// organisation classifies exactly as its forward-slash spelling does.
		// These pin that the fix split the path rather than blanket-refusing.
		{
			name: "a backslash-delimited read on the pinned organisation",
			req:  adoReq(http.MethodGet, `/acme\proj\_apis\git\repositories`, ""),
			want: CapRead,
		},
		{
			name: "a backslash-delimited denied area is still denied, not refused",
			req:  adoReq(http.MethodGet, `/acme\_apis\tokens\pats`, ""),
			want: CapDeniedTokens,
		},
		{
			name: "a backslash-delimited work-item write",
			req:  adoReq(http.MethodPatch, `/acme/proj\_apis\wit\workitems\12`, `{}`),
			want: CapWorkWrite,
		},
	})
}

// MULTIPLY-ENCODED STRUCTURE. "%252F" decodes once to the literal text "%2F",
// which is harmless to a service that decodes once — and a separator to any
// layer between here and the service that decodes one more time. Whether such
// a layer exists is not knowable from here, so a segment that decodes, at ANY
// further depth, to a separator or a dot segment is refused. Each separator
// and the dot have a twin, and the depth goes past two so the rule is not
// "decode exactly twice".
func TestEvasionMultiplyEncodedStructure(t *testing.T) {
	runCases(t, []caseT{
		{name: "a double-encoded slash", req: adoReq(http.MethodGet, "/acme/proj%252F..%252F..%252Fevil/_apis/projects", ""), wantErr: true},
		{name: "a double-encoded slash in upper case", req: adoReq(http.MethodGet, "/acme/proj%252f_apis/tokens/pats", ""), wantErr: true},
		{name: "a double-encoded backslash", req: adoReq(http.MethodGet, "/acme/proj%255C..%255C..%255Cevil/_apis/projects", ""), wantErr: true},
		{name: "a double-encoded backslash in lower case", req: adoReq(http.MethodGet, "/acme/proj%255c_apis/tokens/pats", ""), wantErr: true},
		{name: "double-encoded dots", req: adoReq(http.MethodGet, "/acme/%252e%252e/evil/_apis/projects", ""), wantErr: true},
		{name: "double-encoded dots in upper case", req: adoReq(http.MethodGet, "/acme/%252E%252E/evil/_apis/projects", ""), wantErr: true},
		{name: "a TRIPLE-encoded slash", req: adoReq(http.MethodGet, "/acme/proj%25252F_apis/tokens/pats", ""), wantErr: true},
		{name: "a triple-encoded backslash", req: adoReq(http.MethodGet, "/acme/proj%25255C_apis/tokens/pats", ""), wantErr: true},
		{
			// The re-decode must be LENIENT: a strict decoder gives up on the
			// whole segment at "%zz" and calls it harmless, while a lenient one
			// — the kind a request might actually meet — decodes the "%2f".
			name:    "a double-encoded separator beside a malformed escape",
			req:     adoReq(http.MethodGet, "/acme/proj%252f%25zz/_apis/projects", ""),
			wantErr: true,
		},
		{
			name:    "a double-encoded separator inside a $batch operation",
			req:     adoReq(http.MethodPost, "/acme/_apis/wit/$batch", `[{"uri":"/_apis/wit/..%255Chooks/subscriptions"}]`),
			wantErr: true,
		},

		// A literal "%" in a name is NOT structure, and must not be refused:
		// the rule is about what further decoding PRODUCES, not about "%".
		{name: "a repository whose name holds a literal percent sign", req: adoReq(http.MethodGet, "/acme/proj/_apis/git/repositories/100%25", ""), want: CapRead},
		{name: "a name that decodes again to ordinary letters", req: adoReq(http.MethodGet, "/acme/proj/_apis/git/repositories/a%2541", ""), want: CapRead},
		{name: "a literal percent before non-hex", req: adoReq(http.MethodGet, "/acme/proj/_apis/git/repositories/50%25off", ""), want: CapRead},
	})
}

// F2 — THE METHOD OVERRIDE IS TRUSTED DOWNWARD. Microsoft documents the
// override for a POST carrying PATCH or DELETE; whether the service honours a
// GET override is exactly the "not knowable" case this package refuses
// elsewhere. An override may only RAISE a classification.
func TestEvasionMethodOverrideDowngrade(t *testing.T) {
	over := func(method, path, override, body string) Request {
		r := adoReq(method, path, body)
		r.Header = hdr("X-HTTP-Method-Override", override)
		return r
	}
	pushes := "/acme/proj/_apis/git/repositories/r1/pushes"
	runCases(t, []caseT{
		{
			name:    "a POSTed push downgraded to a read",
			req:     over(http.MethodPost, pushes, "GET", `{"refUpdates":[{"name":"refs/heads/main"}]}`),
			wantErr: true,
		},
		{
			name:    "a POSTed work-item write downgraded to a read",
			req:     over(http.MethodPost, "/acme/proj/_apis/wit/workitems/$task", "HEAD", `{}`),
			wantErr: true,
		},
		{
			name:    "an override on a base that is not a POST is not a documented override",
			req:     over(http.MethodGet, "/acme/proj/_apis/git/repositories/r1/pullrequests/7", "PATCH", `{}`),
			wantErr: true,
		},
		{
			name:    "an override on a PATCH base either",
			req:     over(http.MethodPatch, "/acme/proj/_apis/git/repositories/r1/pullrequests/7", "DELETE", `{}`),
			wantErr: true,
		},
		{
			name: "the documented shape still classifies on the override",
			req:  over(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/pullrequests/7", "PATCH", `{"completionOptions":{"bypassPolicy":true}}`),
			want: CapPolicyBypass,
		},
		{
			name: "a POST overridden to DELETE",
			req:  over(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/pullrequests/7", "DELETE", ""),
			want: CapPR,
		},
	})
}

// F3 — A CALLER-CHOSEN SEGMENT MATCHED AS A RESOURCE WORD. The repository
// segment is named by whoever created the repository, so a repository called
// "pullrequestquery" turned every write on it into a read, and one called
// "items" turned repository DELETION into a code write. Resource words count
// only where the route actually puts them.
func TestEvasionResourceNamedRepository(t *testing.T) {
	runCases(t, []caseT{
		{
			name:     "a push to a repository named after the query endpoint is still a push",
			req:      adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/pullrequestquery/pushes", `{"refUpdates":[{"name":"refs/heads/main"}]}`),
			want:     CapCodeWrite,
			wantRefs: []string{"refs/heads/main"},
		},
		{
			name: "deleting a repository named items is repository administration",
			req:  adoReq(http.MethodDelete, "/acme/proj/_apis/git/repositories/items", ""),
			want: CapRepoAdmin,
		},
		{
			name: "deleting a repository named pullrequests is too",
			req:  adoReq(http.MethodDelete, "/acme/proj/_apis/git/repositories/pullrequests", ""),
			want: CapRepoAdmin,
		},
		{
			name: "deleting a repository named refs does not become a ref move",
			req:  adoReq(http.MethodDelete, "/acme/proj/_apis/git/repositories/refs", ""),
			want: CapRepoAdmin,
		},
		{
			name: "the query endpoint still reads where the route puts it",
			req:  adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/pullrequestquery", `{}`),
			want: CapRead,
		},
		{
			name: "the organisation-level pull-request route still classifies",
			req:  adoReq(http.MethodPatch, "/acme/proj/_apis/git/pullrequests/5", `{}`),
			want: CapPR,
		},
	})
}

// F4 — THE $BATCH DOOR WAS NOT PINNED TO THE ORGANISATION. Every operation URI
// is a route in its own right and gets the row's organisation applied to it:
// a first segment other than _apis must be the pinned organisation — see
// TestBatchOperationURIShapes.
func TestEvasionBatchCrossOrg(t *testing.T) {
	batch := "/acme/_apis/wit/$batch"
	runCases(t, []caseT{
		{
			name:    "an operation aimed at another organisation",
			req:     adoReq(http.MethodPost, batch, `[{"uri":"/evil/loot/_apis/wit/workitems/1"}]`),
			wantErr: true,
		},
		{
			name: "an organisation-qualified operation on the pinned organisation",
			req:  adoReq(http.MethodPost, batch, `[{"uri":"/acme/proj/_apis/wit/workitems/2"}]`),
			want: CapWorkWrite,
		},
		{
			name: "an organisation-relative operation",
			req:  adoReq(http.MethodPost, batch, `[{"uri":"/_apis/wit/workitems/1?api-version=7.1"}]`),
			want: CapWorkWrite,
		},
		{
			name:    "an operation whose first segment is another organisation, or an unverified project",
			req:     adoReq(http.MethodPost, batch, `[{"uri":"/evil/_apis/wit/workitems/1"}]`),
			wantErr: true,
		},
		{
			name:    "an operation aimed at another organisation through a backslash",
			req:     adoReq(http.MethodPost, batch, `[{"uri":"/acme\\..\\evil/_apis/wit/workitems/1"}]`),
			wantErr: true,
		},
		{
			name:    "an operation naming another organisation with backslashes throughout",
			req:     adoReq(http.MethodPost, batch, `[{"uri":"\\evil\\loot\\_apis\\wit\\workitems\\1"}]`),
			wantErr: true,
		},
		{
			name: "a backslash-delimited operation on the pinned organisation",
			req:  adoReq(http.MethodPost, batch, `[{"uri":"\\acme\\proj\\_apis\\wit\\workitems\\2"}]`),
			want: CapWorkWrite,
		},
	})
}

// F10 — the token surface is wider than one area name. Every one of these is a
// documented door onto an access token or a session.
func TestEvasionWiderTokenSurface(t *testing.T) {
	runCases(t, []caseT{
		{name: "the singular token area", req: adoReq(http.MethodGet, "/acme/_apis/token/sessiontokens", ""), want: CapDeniedTokens},
		{name: "session token creation", req: adoReq(http.MethodPost, "/acme/_apis/delegatedauth/sessiontokens", `{}`), want: CapDeniedTokens},
		{name: "the web platform auth door", req: adoReq(http.MethodGet, "/acme/_apis/webplatformauth/sessiontoken", ""), want: CapDeniedTokens},
		{name: "the access-token area", req: adoReq(http.MethodGet, "/acme/_apis/accesstokens", ""), want: CapDeniedTokens},
	})
}

// F11 — a header map whose keys were never canonicalised. http.Header.Values
// canonicalises the key it is GIVEN but not the keys already in the map, so a
// caller that built the map by hand (or a proxy that preserved the wire
// spelling) hid the override header from this package entirely.
func TestEvasionNonCanonicalHeaderKey(t *testing.T) {
	runCases(t, []caseT{
		{
			name:    "a lower-case override naming something that is not a method",
			req:     withRawHeader(adoReq(http.MethodPost, "/acme/proj/_apis/wit/workitems/$task", `{}`), "x-http-method-override", "FROB"),
			wantErr: true,
		},
		{
			name:    "a lower-case override downgrading a push",
			req:     withRawHeader(adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/pushes", `{"refUpdates":[{"name":"refs/heads/main"}]}`), "x-http-method-override", "GET"),
			wantErr: true,
		},
		{
			name:    "a lower-case content encoding still hides the body",
			req:     withRawHeader(adoReq(http.MethodPatch, "/acme/proj/_apis/git/repositories/r1/pullrequests/7", `{"status":"completed"}`), "content-encoding", "gzip"),
			wantErr: true,
		},
	})
}

// withRawHeader sets a header under the EXACT key given, bypassing
// http.Header.Set's canonicalisation — the shape a hand-built map has.
func withRawHeader(r Request, key, value string) Request {
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.Header[key] = append(r.Header[key], value)
	return r
}

// F6 — git-over-HTTP. Both verbs used to answer CapUnclassifiedWrite, so a
// consumer could not tell a clone from a push. The transport is OUT OF SCOPE
// for this catalogue and is now refused by name, which a consumer can act on.
func TestGitOverHTTPIsOutOfScope(t *testing.T) {
	runCases(t, []caseT{
		{name: "a push", req: adoReq(http.MethodPost, "/acme/proj/_git/repo/git-receive-pack", "PACK"), wantErr: true},
		{name: "a fetch", req: adoReq(http.MethodPost, "/acme/proj/_git/repo/git-upload-pack", "want"), wantErr: true},
		{name: "the ref advertisement", req: adoReq(http.MethodGet, "/acme/proj/_git/repo/info/refs", ""), wantErr: true},
		{name: "the repository page", req: adoReq(http.MethodGet, "/acme/proj/_git/repo", ""), wantErr: true},
	})
}

// THE $BATCH OPERATION URI SHAPES. An operation URI is accepted relative to
// the organisation ("/_apis/wit/…") or naming the pinned organisation first.
// The project-relative "/Fabrikam-Fiber-Git/_apis/wit/workItems/$Task" of
// Microsoft's WIT batch reference is REFUSED: nothing has shown the batch door
// reads that first segment as a project rather than an organisation (see
// batchOpIsWorkItem). One naming another organisation, or hiding a traversal,
// is refused.
func TestBatchOperationURIShapes(t *testing.T) {
	batch := func(uri string) Request {
		return adoReq(http.MethodPost, "/acme/_apis/wit/$batch", `[{"method":"PATCH","uri":"`+uri+`"}]`)
	}
	runCases(t, []caseT{
		{name: "organisation-relative", req: batch("/_apis/wit/workitems/284?api-version=7.1"), want: CapWorkWrite},
		{name: "organisation named", req: batch("/acme/_apis/wit/workitems/284"), want: CapWorkWrite},
		{name: "organisation and project named", req: batch("/acme/proj/_apis/wit/workitems/284"), want: CapWorkWrite},
		{name: "organisation named in another case", req: batch("/ACME/proj/_apis/wit/workitems/284"), want: CapWorkWrite},

		{name: "project-relative create, resolution unverified", req: batch("/proj/_apis/wit/workitems/$Bug?api-version=7.1"), wantErr: true},
		{name: "project-relative, like the documented example", req: batch("/Fabrikam-Fiber-Git/_apis/wit/workItems/$Task"), wantErr: true},
		{name: "another organisation, organisation-level", req: batch("/evil/_apis/wit/workitems/1"), wantErr: true},
		{name: "another organisation, then a project", req: batch("/evil/loot/_apis/wit/workitems/1"), wantErr: true},
		{name: "another organisation in upper case", req: batch("/EVIL/proj/_apis/wit/workitems/1"), wantErr: true},
		{name: "_apis too deep to be a project route", req: batch("/acme/proj/extra/_apis/wit/workitems/1"), wantErr: true},
		{name: "a project-relative URI outside the work-item area", req: batch("/proj/_apis/git/repositories/r/pushes"), wantErr: true},
		{name: "a dot segment walking out of the project", req: batch("/proj/../evil/_apis/wit/workitems/1"), wantErr: true},
		{name: "an encoded dot segment", req: batch("/proj/%2e%2e/evil/_apis/wit/workitems/1"), wantErr: true},
		{name: "a dot segment walking out of the work-item area", req: batch("/proj/_apis/wit/../hooks/subscriptions"), wantErr: true},
		{name: "an absolute URI", req: batch("https://dev.azure.com/evil/_apis/wit/workitems/1"), wantErr: true},
	})
}

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
