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
// is a route in its own right and gets the row's organisation applied to it.
func TestEvasionBatchCrossOrg(t *testing.T) {
	batch := "/acme/_apis/wit/$batch"
	runCases(t, []caseT{
		{
			name:    "an operation aimed at another organisation",
			req:     adoReq(http.MethodPost, batch, `[{"uri":"/evil/_apis/wit/workitems/1"}]`),
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
