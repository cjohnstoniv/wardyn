// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// hdr is a one-line http.Header literal, since most cases need exactly one.
func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

// adoReq is a request against organisation "acme" on dev.azure.com — the
// shape every case starts from, so a case shows only what it varies.
func adoReq(method, path, body string) Request {
	r := Request{Method: method, Host: "dev.azure.com", Path: path, Org: "acme"}
	if body != "" {
		r.BodyPeek = []byte(body)
	}
	return r
}

// run classifies one case and checks the capability, the refs and whether it
// refused.
type caseT struct {
	name     string
	req      Request
	want     Capability
	wantRefs []string
	wantErr  bool
}

func runCases(t *testing.T, cases []caseT) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Classify(tc.req)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Classify() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				if got.Capability != "" {
					t.Fatalf("a refused request returned capability %q — a refusal must carry none", got.Capability)
				}
				return
			}
			if got.Capability != tc.want {
				t.Fatalf("Classify() = %q, want %q", got.Capability, tc.want)
			}
			if tc.wantRefs != nil && !slices.Equal(got.Refs, tc.wantRefs) {
				t.Fatalf("Refs = %v, want %v", got.Refs, tc.wantRefs)
			}
		})
	}
}

// HAZARD 1 — X-HTTP-Method-Override. Azure DevOps acts on the override, so a
// classifier reading the request line would charge a GET's capability to a
// PATCH.
func TestClassifyMethodOverride(t *testing.T) {
	prPath := "/acme/proj/_apis/git/repositories/r1/pullrequests/7"
	overridden := func(method, path, override, body string) Request {
		r := adoReq(method, path, body)
		r.Header = hdr("X-HTTP-Method-Override", override)
		return r
	}
	runCases(t, []caseT{
		{name: "a plain GET is a read", req: adoReq(http.MethodGet, prPath, ""), want: CapRead},
		{
			name: "a POST overridden to PATCH is classified as the PATCH",
			req:  overridden(http.MethodPost, prPath, "PATCH", `{"status":"completed"}`),
			want: CapPR,
		},
		{
			name: "the override still reaches the body rules",
			req:  overridden(http.MethodPost, prPath, "PATCH", `{"completionOptions":{"bypassPolicy":true}}`),
			want: CapPolicyBypass,
		},
		{
			name: "a lowercase override is still the method",
			req:  overridden(http.MethodPost, prPath, "patch", `{"status":"active"}`),
			want: CapPR,
		},
		{
			name: "a POST overridden to GET is REFUSED — an override may only raise",
			req:  overridden(http.MethodPost, "/acme/proj/_apis/wit/workitems/$task", "GET", ""),

			wantErr: true,
		},
		{
			name:    "an override that is not a method is refused, not ignored",
			req:     overridden(http.MethodPost, prPath, "FROB", ""),
			wantErr: true,
		},
		{
			name: "two override values are refused — which one the server acts on is not knowable",
			req: func() Request {
				r := adoReq(http.MethodPost, prPath, "")
				r.Header = hdr("X-HTTP-Method-Override", "PATCH", "X-HTTP-Method-Override", "DELETE")
				return r
			}(),
			wantErr: true,
		},
		{
			name:    "a request-line method that is not a method is refused",
			req:     adoReq("FROB", prPath, ""),
			wantErr: true,
		},
		{
			name:    "TRACE has no capability",
			req:     adoReq(http.MethodTrace, prPath, ""),
			wantErr: true,
		},
	})
}

// HAZARD 2 — POSTs that only read. Azure DevOps posts queries whose input is
// too large for a query string, so charging them a write would make a
// read-only grant unable to read.
func TestClassifyPostsThatAreReads(t *testing.T) {
	runCases(t, []caseT{
		{name: "a work-item query", req: adoReq(http.MethodPost, "/acme/_apis/wit/wiql", `{"query":"select 1"}`), want: CapRead},
		{name: "a project-scoped work-item query", req: adoReq(http.MethodPost, "/acme/proj/_apis/wit/wiql", ""), want: CapRead},
		{name: "a work-item batch READ", req: adoReq(http.MethodPost, "/acme/_apis/wit/workitemsbatch", ""), want: CapRead},
		{
			name: "a pull-request query",
			req:  adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/pullrequestquery", ""),
			want: CapRead,
		},
		{
			name: "code search on its own host",
			req: func() Request {
				r := adoReq(http.MethodPost, "/acme/proj/_apis/search/codesearchresults", `{"searchText":"x"}`)
				r.Host = "almsearch.dev.azure.com"
				return r
			}(),
			want: CapRead,
		},
		{
			name: "a search area on the ORDINARY host is not a trusted query door",
			req:  adoReq(http.MethodPost, "/acme/proj/_apis/search/codesearchresults", ""),
			want: CapUnclassifiedWrite,
		},
		{
			name: "the internal hierarchy query is denied, not read",
			req:  adoReq(http.MethodPost, "/acme/_apis/Contribution/HierarchyQuery", `{"contributionIds":["x"]}`),
			want: CapDeniedInternal,
		},
		{
			name: "and denied on a GET too",
			req:  adoReq(http.MethodGet, "/acme/_apis/contribution/hierarchyquery", ""),
			want: CapDeniedInternal,
		},
		{
			name: "an ordinary work-item POST is still a write",
			req:  adoReq(http.MethodPost, "/acme/proj/_apis/wit/workitems/$task", `{}`),
			want: CapWorkWrite,
		},
	})
}

// HAZARD 3 — the work-item batch door. It forwards each operation's URI
// internally, so CapWorkWrite is honest only if every operation in it is a
// work-item write.
func TestClassifyWorkItemBatch(t *testing.T) {
	batch := "/acme/proj/_apis/wit/$batch"
	runCases(t, []caseT{
		{
			name: "every operation is a work-item write",
			req: adoReq(http.MethodPost, batch,
				`[{"method":"PATCH","uri":"/_apis/wit/workitems/1?api-version=7.1"},`+
					`{"method":"PATCH","uri":"/acme/proj/_apis/wit/workitems/2"}]`),
			want: CapWorkWrite,
		},
		{
			name: "one operation aimed at another area is refused",
			req: adoReq(http.MethodPost, batch,
				`[{"method":"PATCH","uri":"/_apis/wit/workitems/1"},`+
					`{"method":"POST","uri":"/_apis/git/repositories/r1/pushes"}]`),
			wantErr: true,
		},
		{
			name: "an ABSOLUTE operation URI is refused — it could name another organisation",
			req: adoReq(http.MethodPost, batch,
				`[{"method":"PATCH","uri":"https://dev.azure.com/other/_apis/wit/workitems/1"}]`),
			wantErr: true,
		},
		{
			name:    "a batch naming no operation cannot be classified",
			req:     adoReq(http.MethodPost, batch, `[]`),
			wantErr: true,
		},
		{
			name:    "a batch with no visible body is refused",
			req:     adoReq(http.MethodPost, batch, ""),
			wantErr: true,
		},
		{
			name:    "a batch operation with a repeated key is refused",
			req:     adoReq(http.MethodPost, batch, `[{"uri":"/_apis/wit/workitems/1","uri":"/_apis/git/pushes"}]`),
			wantErr: true,
		},
		{
			name: "an operation URI whose _apis is percent-hidden still reads as the work-item area",
			req:  adoReq(http.MethodPost, batch, `[{"uri":"/%5Fapis/wit/workitems/1"}]`),
			want: CapWorkWrite,
		},
	})
}

// HAZARD 4 — the bounded body peek for a policy-bypassing pull-request
// completion, and the two ways a body can lie about itself.
func TestClassifyPullRequestBodyPeek(t *testing.T) {
	pr := "/acme/proj/_apis/git/repositories/r1/pullrequests/7"
	withHeader := func(r Request, kv ...string) Request {
		r.Header = hdr(kv...)
		return r
	}
	runCases(t, []caseT{
		{name: "an ordinary completion", req: adoReq(http.MethodPatch, pr, `{"status":"completed"}`), want: CapPR},
		{name: "a PATCH with no body", req: adoReq(http.MethodPatch, pr, ""), want: CapPR},
		{name: "a PR delete", req: adoReq(http.MethodDelete, pr, ""), want: CapPR},
		{
			name: "bypassPolicy true is a bypass, not a pull request",
			req:  adoReq(http.MethodPatch, pr, `{"status":"completed","completionOptions":{"bypassPolicy":true,"bypassReason":"hotfix"}}`),
			want: CapPolicyBypass,
		},
		{
			name: "bypassPolicy false is an ordinary pull request",
			req:  adoReq(http.MethodPatch, pr, `{"completionOptions":{"bypassPolicy":false}}`),
			want: CapPR,
		},
		{
			name: "a PR CREATED with the bypass set is a bypass too",
			req:  adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/pullrequests", `{"completionOptions":{"bypassPolicy":true}}`),
			want: CapPolicyBypass,
		},
		{
			name:    "a repeated bypassPolicy key is refused — a last-key-wins parser would read the true",
			req:     adoReq(http.MethodPatch, pr, `{"completionOptions":{"bypassPolicy":false,"bypassReason":"x","bypassPolicy":true}}`),
			wantErr: true,
		},
		{
			name:    "the same trick spelled in another case is refused",
			req:     adoReq(http.MethodPatch, pr, `{"completionOptions":{"bypassPolicy":false,"BYPASSPOLICY":true}}`),
			wantErr: true,
		},
		{
			name:    "a repeated key at the TOP level is refused as well",
			req:     adoReq(http.MethodPatch, pr, `{"completionOptions":{"bypassPolicy":false},"completionOptions":{"bypassPolicy":true}}`),
			wantErr: true,
		},
		{
			name:    "a content encoding hides the body",
			req:     withHeader(adoReq(http.MethodPatch, pr, `{"status":"completed"}`), "Content-Encoding", "gzip"),
			wantErr: true,
		},
		{
			name: "an explicit identity encoding does not",
			req:  withHeader(adoReq(http.MethodPatch, pr, `{"status":"completed"}`), "Content-Encoding", "identity"),
			want: CapPR,
		},
		{
			name:    "a declared length longer than the peek is refused",
			req:     withHeader(adoReq(http.MethodPatch, pr, `{"status":"completed"}`), "Content-Length", "999999"),
			wantErr: true,
		},
		{
			name:    "two Content-Length values are refused",
			req:     withHeader(adoReq(http.MethodPatch, pr, `{}`), "Content-Length", "2", "Content-Length", "99"),
			wantErr: true,
		},
		{
			name: "a peek sitting exactly on the bound may continue, so it is refused",
			req: func() Request {
				r := adoReq(http.MethodPatch, pr, "")
				body := make([]byte, MaxBodyPeek)
				copy(body, `{"completionOptions":{"bypassPolicy":false}`)
				for i := len(`{"completionOptions":{"bypassPolicy":false}`); i < len(body); i++ {
					body[i] = ' '
				}
				r.BodyPeek = body
				return r
			}(),
			wantErr: true,
		},
		{
			name:    "a body that is not JSON at all is refused",
			req:     adoReq(http.MethodPatch, pr, `not json`),
			wantErr: true,
		},
		{
			name:    "a body carrying two JSON documents is refused",
			req:     adoReq(http.MethodPatch, pr, `{"a":1}{"completionOptions":{"bypassPolicy":true}}`),
			wantErr: true,
		},
	})
}

// HAZARD 5 — a ref move onto a policy-protected branch. Azure DevOps asks for
// the same write scope either way, so the ref names in the body are the only
// place the difference exists.
func TestClassifyRefMove(t *testing.T) {
	refs := "/acme/proj/_apis/git/repositories/r1/refs"
	pushes := "/acme/proj/_apis/git/repositories/r1/pushes"
	protect := func(r Request, names ...string) Request {
		r.RefProtected = func(ref string) bool { return slices.Contains(names, ref) }
		return r
	}
	refsBody := `[{"name":"refs/heads/feature/x","oldObjectId":"0","newObjectId":"1"}]`
	mainBody := `[{"name":"refs/heads/main","oldObjectId":"0","newObjectId":"1"}]`
	pushBody := `{"refUpdates":[{"name":"refs/heads/main"}],"commits":[{"comment":"c"}]}`
	runCases(t, []caseT{
		{
			name:     "an unprotected ref move is a code write, and the refs come back",
			req:      protect(adoReq(http.MethodPost, refs, refsBody), "refs/heads/main"),
			want:     CapCodeWrite,
			wantRefs: []string{"refs/heads/feature/x"},
		},
		{
			name:     "a protected ref move is a POLICY BYPASS, not a code write",
			req:      protect(adoReq(http.MethodPost, refs, mainBody), "refs/heads/main"),
			want:     CapPolicyBypass,
			wantRefs: []string{"refs/heads/main"},
		},
		{
			name:     "with no oracle the refs still come back, for the caller's own cache",
			req:      adoReq(http.MethodPost, refs, mainBody),
			want:     CapCodeWrite,
			wantRefs: []string{"refs/heads/main"},
		},
		{
			name:     "a push body carries its refs under refUpdates",
			req:      protect(adoReq(http.MethodPost, pushes, pushBody), "refs/heads/main"),
			want:     CapPolicyBypass,
			wantRefs: []string{"refs/heads/main"},
		},
		{
			name:     "an unprotected push",
			req:      protect(adoReq(http.MethodPost, pushes, `{"refUpdates":[{"name":"refs/heads/topic"}]}`), "refs/heads/main"),
			want:     CapCodeWrite,
			wantRefs: []string{"refs/heads/topic"},
		},
		{
			name: "one protected ref among several is enough",
			req: protect(adoReq(http.MethodPost, refs,
				`[{"name":"refs/heads/topic"},{"name":"refs/heads/main"}]`), "refs/heads/main"),
			want:     CapPolicyBypass,
			wantRefs: []string{"refs/heads/topic", "refs/heads/main"},
		},
		{
			name:    "a ref name spelled with backslashes cannot be checked against the cache",
			req:     protect(adoReq(http.MethodPost, refs, `[{"name":"refs\\heads\\main"}]`), "refs/heads/main"),
			wantErr: true,
		},
		{
			name:    "and its push twin",
			req:     protect(adoReq(http.MethodPost, pushes, `{"refUpdates":[{"name":"refs\\heads\\main"}]}`), "refs/heads/main"),
			wantErr: true,
		},
		{
			name:    "one backslashed ref among plain ones still refuses the whole update",
			req:     protect(adoReq(http.MethodPost, refs, `[{"name":"refs/heads/topic"},{"name":"refs/heads\\main"}]`), "refs/heads/main"),
			wantErr: true,
		},
		{name: "a ref update naming no branch cannot be gated", req: adoReq(http.MethodPost, refs, `[]`), wantErr: true},
		{name: "a ref update with no body cannot be gated", req: adoReq(http.MethodPost, refs, ""), wantErr: true},
		{
			name:    "a ref update with a repeated key is refused",
			req:     adoReq(http.MethodPost, refs, `[{"name":"refs/heads/topic","name":"refs/heads/main"}]`),
			wantErr: true,
		},
		// Update Ref (PATCH refs?filter=heads/main) names its ref in the query,
		// which Request.Path never carries: a body naming an unprotected ref must
		// not make a branch lock a code write.
		{
			name: "a PATCH on refs is policy_bypass whatever ref its body names",
			req:  adoReq(http.MethodPatch, refs, `{"refUpdates":[{"name":"refs/heads/feature/x"}],"isLocked":true}`),
			want: CapPolicyBypass,
		},
		{name: "a PATCH on refs with the documented lock body", req: adoReq(http.MethodPatch, refs, `{"isLocked":true}`), want: CapPolicyBypass},
		{name: "a PATCH on refs with no body", req: adoReq(http.MethodPatch, refs, ""), want: CapPolicyBypass},
		{name: "a PUT on refs is held to the same rule", req: adoReq(http.MethodPut, refs, `{"isLocked":true}`), want: CapPolicyBypass},
		{
			name: "a POST raised to PATCH by an override is policy_bypass too",
			req: func() Request {
				r := adoReq(http.MethodPost, refs, refsBody)
				r.Header = hdr("X-HTTP-Method-Override", "PATCH")
				return r
			}(),
			want: CapPolicyBypass,
		},
	})
}

// HAZARD 6 — organisation pinning. A row admits ONE organisation, and a
// request to another is refused rather than classified: a capability answered
// for the wrong organisation is a token minted for a tenant nobody authored.
func TestClassifyOrgPinning(t *testing.T) {
	onHost := func(host, path, org string) Request {
		return Request{Method: http.MethodGet, Host: host, Path: path, Org: org}
	}
	runCases(t, []caseT{
		{name: "the pinned organisation", req: onHost("dev.azure.com", "/acme/proj/_apis/git/repositories", "acme"), want: CapRead},
		{
			name: "the comparison folds case, because Azure DevOps' own URLs do",
			req:  onHost("dev.azure.com", "/ACME/proj/_apis/git/repositories", "acme"),
			want: CapRead,
		},
		{name: "a pinned organisation written in another case", req: onHost("dev.azure.com", "/acme/_apis/projects", "ACME"), want: CapRead},
		{name: "another organisation is refused", req: onHost("dev.azure.com", "/other/proj/_apis/git/repositories", "acme"), wantErr: true},
		{name: "another organisation behind a backslash is refused", req: onHost("dev.azure.com", `\other\proj/_apis/git/repositories`, "acme"), wantErr: true},
		{name: "a backslash-led path still names the pinned organisation", req: onHost("dev.azure.com", `\acme\proj\_apis\git\repositories`, "acme"), want: CapRead},
		{name: "a legacy host names the organisation in the label", req: onHost("acme.visualstudio.com", "/proj/_apis/git/repositories", "acme"), want: CapRead},
		{name: "a legacy host for another organisation is refused", req: onHost("other.visualstudio.com", "/proj/_apis/git/repositories", "acme"), wantErr: true},
		{name: "a legacy service subdomain still names it first", req: onHost("acme.vssps.visualstudio.com", "/_apis/graph/users", "acme"), want: CapRead},
		{name: "a service subdomain of dev.azure.com keeps the path organisation", req: onHost("vsrm.dev.azure.com", "/acme/proj/_apis/release/releases", "acme"), want: CapRead},
		{name: "a path naming no organisation is refused", req: onHost("dev.azure.com", "/", "acme"), wantErr: true},
		{name: "an unpinned request is refused", req: onHost("dev.azure.com", "/acme/_apis/projects", ""), wantErr: true},
		{name: "a host that is not Azure DevOps is refused", req: onHost("github.com", "/acme/repo", "acme"), wantErr: true},
		{name: "an Azure DevOps SERVER host is refused — it takes no Entra token", req: onHost("tfs.corp.example", "/acme/_apis/projects", "acme"), wantErr: true},
		{name: "a host that merely ends in the right letters is refused", req: onHost("evildev.azure.com.attacker.test", "/acme/_apis/projects", "acme"), wantErr: true},
	})
}

// TestClassifyDeniedAreas pins the areas no capability can be held over, for
// READS as much as writes — a GET of the token area lists an organisation's
// personal access tokens.
func TestClassifyDeniedAreas(t *testing.T) {
	runCases(t, []caseT{
		{name: "listing tokens", req: adoReq(http.MethodGet, "/acme/_apis/tokens/pats", ""), want: CapDeniedTokens},
		{name: "minting a token", req: adoReq(http.MethodPost, "/acme/_apis/tokens/pats", `{}`), want: CapDeniedTokens},
		{name: "token administration", req: adoReq(http.MethodGet, "/acme/_apis/tokenadmin/personalaccesstokens/u1", ""), want: CapDeniedTokens},
		{name: "a service hook subscription", req: adoReq(http.MethodPost, "/acme/_apis/hooks/subscriptions", `{}`), want: CapDeniedServiceHooks},
		{name: "the servicehooks spelling", req: adoReq(http.MethodPost, "/acme/_apis/servicehooks/subscriptions", `{}`), want: CapDeniedServiceHooks},
		{name: "installing an extension", req: adoReq(http.MethodPost, "/acme/_apis/extensionmanagement/installedextensions", `{}`), want: CapDeniedExtensions},
		{
			name: "a percent-hidden _apis does NOT launder the token area into the read floor",
			req:  adoReq(http.MethodGet, "/acme/%5Fapis/tokens/pats", ""),
			want: CapDeniedTokens,
		},
		{
			name: "a percent-hidden _apis behind a backslash is still the token area",
			req:  adoReq(http.MethodGet, `/acme/%5Fapis\tokens\pats`, ""),
			want: CapDeniedTokens,
		},
		{
			name:    "a segment that decodes into two segments is refused, not guessed",
			req:     adoReq(http.MethodGet, "/acme/proj%2F_apis/tokens/pats", ""),
			wantErr: true,
		},
		{
			name:    "and its backslash twin — an encoded backslash is a separator too",
			req:     adoReq(http.MethodGet, "/acme/proj%5C_apis/tokens/pats", ""),
			wantErr: true,
		},
		{
			name:    "and in lower case",
			req:     adoReq(http.MethodGet, "/acme/proj%5c_apis/tokens/pats", ""),
			wantErr: true,
		},
		{
			name:    "an undecodable segment is refused",
			req:     adoReq(http.MethodGet, "/acme/%zz/_apis/projects", ""),
			wantErr: true,
		},
	})
}

// TestClassifyWriteAreas is the area table: every grantable write capability
// reached through a real route, and the fail-closed answer for an area this
// catalogue has no opinion about.
func TestClassifyWriteAreas(t *testing.T) {
	onHost := func(host string, r Request) Request { r.Host = host; return r }
	runCases(t, []caseT{
		{name: "a branch policy", req: adoReq(http.MethodPost, "/acme/proj/_apis/policy/configurations", `{}`), want: CapPolicyAdmin},
		{name: "a repository policy under the git area", req: adoReq(http.MethodPost, "/acme/proj/_apis/git/policy/configurations", `{}`), want: CapPolicyAdmin},
		{name: "creating a repository", req: adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories", `{}`), want: CapRepoAdmin},
		{name: "deleting a repository", req: adoReq(http.MethodDelete, "/acme/proj/_apis/git/repositories/r1", ""), want: CapRepoAdmin},
		{name: "writing a file", req: adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/items", `{}`), want: CapCodeWrite},
		{name: "reverting a commit", req: adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/reverts", `{}`), want: CapCodeWrite},
		{name: "an annotated tag is a ref move", req: adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/annotatedtags", `{}`), want: CapPolicyBypass},
		{name: "a fork sync is a ref move", req: adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/forksyncrequests", `{}`), want: CapPolicyBypass},
		{name: "an import replaces the repository", req: adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/importrequests", `{}`), want: CapRepoAdmin},
		{name: "an access control list", req: adoReq(http.MethodPost, "/acme/_apis/accesscontrollists/ns1", `{}`), want: CapSecurityAdmin},
		{name: "a directory identity", req: onHost("vssps.dev.azure.com", adoReq(http.MethodPost, "/acme/_apis/graph/users", `{}`)), want: CapSecurityAdmin},
		{name: "a service connection", req: adoReq(http.MethodPost, "/acme/proj/_apis/serviceendpoint/endpoints", `{}`), want: CapServiceEndpointAdmin},
		{name: "queueing a build", req: adoReq(http.MethodPost, "/acme/proj/_apis/build/builds", `{}`), want: CapBuildExecute},
		{name: "editing a build definition", req: adoReq(http.MethodPut, "/acme/proj/_apis/build/definitions/3", `{}`), want: CapBuildAdmin},
		{name: "running a pipeline", req: adoReq(http.MethodPost, "/acme/proj/_apis/pipelines/12/runs", `{}`), want: CapBuildExecute},
		{name: "creating a pipeline", req: adoReq(http.MethodPost, "/acme/proj/_apis/pipelines", `{}`), want: CapBuildAdmin},
		{name: "a classic release", req: onHost("vsrm.dev.azure.com", adoReq(http.MethodPost, "/acme/proj/_apis/release/releases", `{}`)), want: CapBuildExecute},
		{name: "a release definition", req: onHost("vsrm.dev.azure.com", adoReq(http.MethodPut, "/acme/proj/_apis/release/definitions/2", `{}`)), want: CapBuildAdmin},
		{name: "a work item", req: adoReq(http.MethodPatch, "/acme/proj/_apis/wit/workitems/12", `{}`), want: CapWorkWrite},
		{name: "a wiki page", req: adoReq(http.MethodPut, "/acme/proj/_apis/wiki/wikis/w1/pages", `{}`), want: CapWikiWrite},
		{name: "publishing a package", req: onHost("pkgs.dev.azure.com", adoReq(http.MethodPut, "/acme/_apis/packaging/feeds/f1/npm/p/-/p-1.0.0.tgz", `{}`)), want: CapPackagingWrite},
		{name: "an npm publish on the feed route", req: onHost("pkgs.dev.azure.com", adoReq(http.MethodPut, "/acme/_packaging/f1/npm/registry/p", `{}`)), want: CapPackagingWrite},
		{name: "a project-scoped nuget push", req: onHost("pkgs.dev.azure.com", adoReq(http.MethodPut, "/acme/proj/_packaging/f1/nuget/v2", `{}`)), want: CapPackagingWrite},
		{name: "a pypi upload", req: onHost("pkgs.dev.azure.com", adoReq(http.MethodPost, "/acme/_packaging/f1/pypi/upload", `{}`)), want: CapPackagingWrite},
		{name: "a maven deploy", req: onHost("pkgs.dev.azure.com", adoReq(http.MethodPut, "/acme/proj/_packaging/f1/maven/v1/g/a/1/a-1.jar", `{}`)), want: CapPackagingWrite},
		{name: "a universal package delete", req: onHost("pkgs.dev.azure.com", adoReq(http.MethodDelete, "/acme/_packaging/f1/upack/packages/p/versions/1", "")), want: CapPackagingWrite},
		{name: "a feed-route PATCH on the legacy host", req: onHost("acme.pkgs.visualstudio.com", adoReq(http.MethodPatch, "/_packaging/f1/npm/registry/p", `{}`)), want: CapPackagingWrite},
		{name: "a feed-route read", req: onHost("pkgs.dev.azure.com", adoReq(http.MethodGet, "/acme/_packaging/f1/npm/registry/p", "")), want: CapRead},
		{name: "a feed route with an unknown protocol", req: onHost("pkgs.dev.azure.com", adoReq(http.MethodPut, "/acme/_packaging/f1/cargo/p", `{}`)), want: CapUnclassifiedWrite},
		{name: "a feed route off the packages host", req: adoReq(http.MethodPut, "/acme/_packaging/f1/npm/registry/p", `{}`), want: CapUnclassifiedWrite},
		{name: "_packaging deeper than a project", req: onHost("pkgs.dev.azure.com", adoReq(http.MethodPut, "/acme/proj/x/_packaging/f1/npm/p", `{}`)), want: CapUnclassifiedWrite},
		{name: "creating a project", req: adoReq(http.MethodPost, "/acme/_apis/projects", `{}`), want: CapProjectAdmin},
		{name: "an area with no opinion is refused, not guessed", req: adoReq(http.MethodPost, "/acme/proj/_apis/distributedtask/pools", `{}`), want: CapUnclassifiedWrite},
		{name: "a write with no _apis at all", req: adoReq(http.MethodPost, "/acme/proj/_admin/whatever", `{}`), want: CapUnclassifiedWrite},
		{name: "an ordinary read", req: adoReq(http.MethodGet, "/acme/proj/_apis/git/repositories/r1/items", ""), want: CapRead},
		{name: "a HEAD is a read", req: adoReq(http.MethodHead, "/acme/proj/_apis/build/definitions", ""), want: CapRead},
		{name: "an OPTIONS is a read", req: adoReq(http.MethodOptions, "/acme/_apis/projects", ""), want: CapRead},
	})
}

// TestUnclassifiedWriteIsNotGrantable is the fail-closed contract in one
// assertion: the answer a caller gets for a write this catalogue does not
// recognize can never be turned into a scope.
func TestUnclassifiedWriteIsNotGrantable(t *testing.T) {
	v, err := Classify(adoReq(http.MethodPost, "/acme/proj/_apis/distributedtask/pools", `{}`))
	if err != nil {
		t.Fatalf("Classify() error = %v", err)
	}
	if v.Capability.Grantable() {
		t.Fatalf("%q is grantable — an unrecognized write must never be", v.Capability)
	}
	if _, err := ScopesFor([]Capability{v.Capability}); err == nil {
		t.Fatalf("ScopesFor(%q) = nil error — an empty scope set reads as permission to a subset gate", v.Capability)
	}
	if Permits(GrantableCapabilities(), v) {
		t.Fatalf("Permits() allowed %q against EVERY grantable capability", v.Capability)
	}
}

// TestDeniedAreasAreNeverGrantable is the same contract for the refused areas.
func TestDeniedAreasAreNeverGrantable(t *testing.T) {
	for _, c := range []Capability{CapDeniedTokens, CapDeniedServiceHooks, CapDeniedExtensions, CapDeniedInternal} {
		if c.Grantable() {
			t.Errorf("%q is grantable — a denied area must never be", c)
		}
		if _, err := ScopesFor([]Capability{c}); err == nil {
			t.Errorf("ScopesFor(%q) = nil error — a denied area must never resolve to a scope set", c)
		}
		if Permits(GrantableCapabilities(), Verdict{Capability: c}) {
			t.Errorf("Permits() allowed %q against EVERY grantable capability", c)
		}
		if !strings.HasPrefix(Label(c), "Not available") {
			t.Errorf("Label(%q) = %q, want a label that reads as unavailable", c, Label(c))
		}
	}
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

// A read the minted token cannot perform. Reads are an enumerated table: an
// area outside it is refused as unclassified, and every area in it carries its
// read scope. A read floor that answered CapRead for any area would classify
// an area with no read scope as a read that 403s at the forge.
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
