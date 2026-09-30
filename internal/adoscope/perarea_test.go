// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"net/http"
	"testing"
)

// perAreaCase is one row of the per-area golden table. want is a wire name
// rather than a constant, so the table reads as the catalogue a reviewer
// checks against Azure DevOps' own REST reference; "" with wantErr means
// Classify refuses the request.
type perAreaCase struct {
	name    string
	req     Request
	want    Capability
	wantErr bool
}

// onHost is adoReq on another Azure DevOps service host.
func onHost(host, method, path, body string) Request {
	r := adoReq(method, path, body)
	r.Host = host
	return r
}

// withQuery is r with a raw query string.
func withQuery(r Request, q string) Request {
	r.RawQuery = q
	return r
}

// perAreaCases is the golden table: one row per area × method class. It is
// also the corpus TestSplitEquivalence replays against the verdicts the
// catalogue gave before the per-area split (testdata/pre-split-verdicts.json).
var perAreaCases = []perAreaCase{
	// ---- Repos ----
	{name: "git GET", req: adoReq(http.MethodGet, "/acme/proj/_apis/git/repositories/r1/items", ""), want: "code_read"},
	{name: "git pullrequestquery POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/pullrequestquery", `{}`), want: "code_read"},
	{name: "policy GET", req: adoReq(http.MethodGet, "/acme/proj/_apis/policy/configurations", ""), want: "code_read"},
	{name: "policy configurations POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/policy/configurations", `{}`), want: "policy_admin"},
	{name: "almsearch codesearchresults POST", req: onHost("almsearch.dev.azure.com", http.MethodPost, "/acme/proj/_apis/search/codesearchresults", `{}`), want: "code_read"},
	{name: "almsearch workitemsearchresults POST", req: onHost("almsearch.dev.azure.com", http.MethodPost, "/acme/proj/_apis/search/workitemsearchresults", `{}`), want: "work_read"},
	{name: "almsearch wikisearchresults POST", req: onHost("almsearch.dev.azure.com", http.MethodPost, "/acme/proj/_apis/search/wikisearchresults", `{}`), want: "wiki_read"},
	{name: "almsearch undocumented resource POST", req: onHost("almsearch.dev.azure.com", http.MethodPost, "/acme/_apis/search/packagesearchresults", `{}`), want: "unclassified_read"},
	{name: "almsearch undocumented resource GET", req: onHost("almsearch.dev.azure.com", http.MethodGet, "/acme/_apis/search/status", ""), want: "unclassified_read"},
	{name: "git push POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/pushes", `{"refUpdates":[{"name":"refs/heads/wardyn/run1"}]}`), want: "code_write"},
	{name: "pull request POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/git/repositories/r1/pullrequests", `{}`), want: "pr"},
	{name: "pull request bypass PATCH", req: adoReq(http.MethodPatch, "/acme/proj/_apis/git/repositories/r1/pullrequests/7", `{"completionOptions":{"bypassPolicy":true}}`), want: "policy_bypass"},
	{name: "repository DELETE", req: adoReq(http.MethodDelete, "/acme/proj/_apis/git/repositories/r1", ""), want: "repo_admin"},
	{name: "git-over-HTTP is refused", req: adoReq(http.MethodGet, "/acme/proj/_git/r1/info/refs", ""), wantErr: true},

	// ---- Boards ----
	{name: "wit GET", req: adoReq(http.MethodGet, "/acme/proj/_apis/wit/workitems/1", ""), want: "work_read"},
	{name: "wiql POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/wit/wiql", `{}`), want: "work_read"},
	{name: "workitemsbatch POST", req: adoReq(http.MethodPost, "/acme/_apis/wit/workitemsbatch", `{}`), want: "work_read"},
	{name: "work GET", req: adoReq(http.MethodGet, "/acme/proj/team/_apis/work/teamsettings", ""), want: "work_read"},
	{name: "work item PATCH", req: adoReq(http.MethodPatch, "/acme/proj/_apis/wit/workitems/1", `[]`), want: "work_write"},
	{name: "work item create POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/wit/workitems/$task", `[]`), want: "work_write"},
	{name: "work item DELETE", req: adoReq(http.MethodDelete, "/acme/proj/_apis/wit/workitems/1", ""), want: "work_admin"},
	{name: "work item DELETE destroy", req: withQuery(adoReq(http.MethodDelete, "/acme/proj/_apis/wit/workitems/1", ""), "destroy=true"), want: "work_admin"},
	{name: "work items DELETE by ids", req: withQuery(adoReq(http.MethodDelete, "/acme/proj/_apis/wit/workitems", ""), "ids=1,2"), want: "work_admin"},
	{name: "work item comment DELETE", req: adoReq(http.MethodDelete, "/acme/proj/_apis/wit/workitems/1/comments/2", ""), want: "work_write"},
	{name: "workitemsdelete POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/wit/workitemsdelete", `{}`), want: "work_admin"},
	{name: "recyclebin DELETE", req: adoReq(http.MethodDelete, "/acme/proj/_apis/wit/recyclebin/1", ""), want: "work_admin"},
	{name: "recyclebin PATCH", req: adoReq(http.MethodPatch, "/acme/proj/_apis/wit/recyclebin/1", `{}`), want: "work_admin"},
	{name: "classificationnodes POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/wit/classificationnodes/areas", `{}`), want: "work_admin"},
	{name: "field DELETE", req: adoReq(http.MethodDelete, "/acme/_apis/wit/fields/custom.x", ""), want: "work_admin"},
	{name: "tag PATCH", req: adoReq(http.MethodPatch, "/acme/proj/_apis/wit/tags/x", `{}`), want: "work_admin"},
	{name: "saved query POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/wit/queries/shared", `{}`), want: "work_write"},
	{name: "batch of PATCHes", req: adoReq(http.MethodPost, "/acme/_apis/wit/$batch",
		`[{"method":"PATCH","uri":"/_apis/wit/workitems/1"},{"method":"PATCH","uri":"/_apis/wit/workitems/2"}]`), want: "work_write"},
	{name: "batch with one DELETE", req: adoReq(http.MethodPost, "/acme/_apis/wit/$batch",
		`[{"method":"PATCH","uri":"/_apis/wit/workitems/1"},{"method":"DELETE","uri":"/_apis/wit/workitems/2"}]`), want: "work_admin"},
	{name: "batch touching the recycle bin", req: adoReq(http.MethodPost, "/acme/_apis/wit/$batch",
		`[{"method":"PATCH","uri":"/_apis/wit/recyclebin/2"}]`), want: "work_admin"},
	{name: "batch op with an override header", req: adoReq(http.MethodPost, "/acme/_apis/wit/$batch",
		`[{"method":"PATCH","uri":"/_apis/wit/workitems/1","headers":{"x-http-method-override":"DELETE"}}]`), wantErr: true},
	{name: "batch op with no method", req: adoReq(http.MethodPost, "/acme/_apis/wit/$batch",
		`[{"uri":"/_apis/wit/workitems/1"}]`), wantErr: true},
	{name: "batch op with an unknown method", req: adoReq(http.MethodPost, "/acme/_apis/wit/$batch",
		`[{"method":"FROB","uri":"/_apis/wit/workitems/1"}]`), wantErr: true},

	// ---- Wiki ----
	{name: "wiki GET", req: adoReq(http.MethodGet, "/acme/proj/_apis/wiki/wikis/w/pages", ""), want: "wiki_read"},
	{name: "wiki PUT", req: adoReq(http.MethodPut, "/acme/proj/_apis/wiki/wikis/w/pages", `{}`), want: "wiki_write"},

	// ---- Pipelines ----
	{name: "build GET", req: adoReq(http.MethodGet, "/acme/proj/_apis/build/builds/1/logs", ""), want: "build_read"},
	{name: "pipelines GET", req: adoReq(http.MethodGet, "/acme/proj/_apis/pipelines/1/runs", ""), want: "build_read"},
	{name: "build queue POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/build/builds", `{}`), want: "build_execute"},
	{name: "build definition POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/build/definitions", `{}`), want: "build_admin"},
	{name: "pipeline run POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/pipelines/1/runs", `{}`), want: "build_execute"},
	{name: "pipeline create POST", req: adoReq(http.MethodPost, "/acme/proj/_apis/pipelines", `{}`), want: "build_admin"},
	{name: "release GET", req: onHost("vsrm.dev.azure.com", http.MethodGet, "/acme/proj/_apis/release/releases", ""), want: "release_read"},
	{name: "release create POST", req: onHost("vsrm.dev.azure.com", http.MethodPost, "/acme/proj/_apis/release/releases", `{}`), want: "release_execute"},
	{name: "release deployment PATCH", req: onHost("vsrm.dev.azure.com", http.MethodPatch, "/acme/proj/_apis/release/deployments/1", `{}`), want: "release_execute"},
	{name: "release approval PATCH", req: onHost("vsrm.dev.azure.com", http.MethodPatch, "/acme/proj/_apis/release/approvals/1", `{}`), want: "release_admin"},
	{name: "release definition POST", req: onHost("vsrm.dev.azure.com", http.MethodPost, "/acme/proj/_apis/release/definitions", `{}`), want: "release_admin"},
	{name: "serviceendpoint GET", req: adoReq(http.MethodGet, "/acme/proj/_apis/serviceendpoint/endpoints", ""), want: "serviceendpoint_read"},
	{name: "serviceendpoint POST", req: adoReq(http.MethodPost, "/acme/_apis/serviceendpoint/endpoints", `{}`), want: "serviceendpoint_admin"},
	{name: "variablegroups GET", req: adoReq(http.MethodGet, "/acme/proj/_apis/distributedtask/variablegroups", ""), want: "library_read"},
	{name: "securefiles GET", req: adoReq(http.MethodGet, "/acme/proj/_apis/distributedtask/securefiles", ""), want: "library_read"},
	{name: "variablegroups POST stays unclassified", req: adoReq(http.MethodPost, "/acme/_apis/distributedtask/variablegroups", `{}`), want: "unclassified_write"},

	// ---- Artifacts ----
	{name: "packaging feeds GET", req: onHost("feeds.dev.azure.com", http.MethodGet, "/acme/_apis/packaging/feeds", ""), want: "packaging_read"},
	{name: "npm tarball GET", req: onHost("pkgs.dev.azure.com", http.MethodGet, "/acme/_packaging/feed/npm/registry/pkg/-/pkg-1.0.0.tgz", ""), want: "packaging_read"},
	{name: "npm publish PUT", req: onHost("pkgs.dev.azure.com", http.MethodPut, "/acme/_packaging/feed/npm/registry/pkg", `{}`), want: "packaging_write"},
	{name: "package client DELETE", req: onHost("pkgs.dev.azure.com", http.MethodDelete, "/acme/_packaging/feed/npm/registry/pkg/-/pkg-1.0.0.tgz", ""), want: "packaging_manage"},
	{name: "package version PATCH", req: onHost("pkgs.dev.azure.com", http.MethodPatch, "/acme/_apis/packaging/feeds/f/npm/p/versions/1", `{}`), want: "packaging_write"},
	{name: "package version DELETE", req: onHost("pkgs.dev.azure.com", http.MethodDelete, "/acme/_apis/packaging/feeds/f/npm/p/versions/1", ""), want: "packaging_manage"},
	{name: "package recycle bin restore PATCH", req: onHost("pkgs.dev.azure.com", http.MethodPatch, "/acme/_apis/packaging/feeds/f/npm/recyclebin/packages/p/versions/1", `{}`), want: "packaging_write"},
	{name: "feed create POST", req: onHost("feeds.dev.azure.com", http.MethodPost, "/acme/_apis/packaging/feeds", `{}`), want: "packaging_manage"},
	{name: "feed DELETE", req: onHost("feeds.dev.azure.com", http.MethodDelete, "/acme/_apis/packaging/feeds/f", ""), want: "packaging_manage"},
	{name: "feed permissions PATCH", req: onHost("feeds.dev.azure.com", http.MethodPatch, "/acme/_apis/packaging/feeds/f/permissions", `{}`), want: "packaging_manage"},
	{name: "feed recycle bin PATCH", req: onHost("feeds.dev.azure.com", http.MethodPatch, "/acme/_apis/packaging/feedrecyclebin/f", `{}`), want: "packaging_manage"},

	// ---- Test Plans ----
	{name: "test GET", req: adoReq(http.MethodGet, "/acme/proj/_apis/test/runs", ""), want: "test_read"},
	{name: "testplan GET", req: adoReq(http.MethodGet, "/acme/proj/_apis/testplan/plans", ""), want: "test_read"},
	{name: "testresults GET", req: onHost("vstmr.dev.azure.com", http.MethodGet, "/acme/proj/_apis/testresults/runs", ""), want: "test_read"},
	{name: "test POST stays unclassified", req: adoReq(http.MethodPost, "/acme/proj/_apis/test/runs", `{}`), want: "unclassified_write"},

	// ---- Organization ----
	{name: "projects GET", req: adoReq(http.MethodGet, "/acme/_apis/projects", ""), want: "project_read"},
	{name: "projectcollections GET", req: adoReq(http.MethodGet, "/acme/_apis/projectcollections", ""), want: "project_read"},
	{name: "profile GET", req: onHost("vssps.dev.azure.com", http.MethodGet, "/acme/_apis/profile/profiles/me", ""), want: "project_read"},
	{name: "accounts GET", req: onHost("vssps.dev.azure.com", http.MethodGet, "/acme/_apis/accounts", ""), want: "project_read"},
	{name: "graph GET", req: onHost("vssps.dev.azure.com", http.MethodGet, "/acme/_apis/graph/users", ""), want: "identity_read"},
	{name: "identities GET", req: onHost("vssps.dev.azure.com", http.MethodGet, "/acme/_apis/identities", ""), want: "identity_read"},
	{name: "userentitlements GET", req: onHost("vsaex.dev.azure.com", http.MethodGet, "/acme/_apis/userentitlements", ""), want: "identity_read"},
	{name: "groupentitlements GET", req: onHost("vsaex.dev.azure.com", http.MethodGet, "/acme/_apis/groupentitlements", ""), want: "identity_read"},
	{name: "memberentitlements GET", req: onHost("vsaex.dev.azure.com", http.MethodGet, "/acme/_apis/memberentitlements", ""), want: "identity_read"},
	{name: "analytics GET", req: adoReq(http.MethodGet, "/acme/_apis/analytics/views", ""), want: "analytics_read"},
	{name: "project create POST", req: adoReq(http.MethodPost, "/acme/_apis/projects", `{}`), want: "project_admin"},
	{name: "graph group POST", req: onHost("vssps.dev.azure.com", http.MethodPost, "/acme/_apis/graph/groups", `{}`), want: "security_admin"},
	{name: "access control entries POST", req: adoReq(http.MethodPost, "/acme/_apis/accesscontrolentries/ns", `{}`), want: "security_admin"},

	// ---- Discovery ----
	{name: "connectiondata GET", req: adoReq(http.MethodGet, "/acme/_apis/connectiondata", ""), want: "discovery"},
	{name: "resourceareas GET", req: adoReq(http.MethodGet, "/acme/_apis/resourceareas", ""), want: "discovery"},
	{name: "OPTIONS on an area", req: adoReq(http.MethodOptions, "/acme/_apis/wit", ""), want: "discovery"},
	{name: "OPTIONS on the API root", req: adoReq(http.MethodOptions, "/acme/_apis", ""), want: "discovery"},
	{name: "OPTIONS past an area's location", req: adoReq(http.MethodOptions, "/acme/_apis/wit/workitems", ""), want: "unclassified_read"},

	// ---- Refused and unclassified ----
	{name: "tokens GET", req: adoReq(http.MethodGet, "/acme/_apis/tokens/pats", ""), want: "denied_tokens"},
	{name: "tokenadmin POST", req: adoReq(http.MethodPost, "/acme/_apis/tokenadmin/revocations", `{}`), want: "denied_tokens"},
	{name: "hooks GET", req: adoReq(http.MethodGet, "/acme/_apis/hooks/subscriptions", ""), want: "denied_service_hooks"},
	{name: "extensionmanagement GET", req: adoReq(http.MethodGet, "/acme/_apis/extensionmanagement/installedextensions", ""), want: "denied_extension_management"},
	{name: "contribution POST", req: adoReq(http.MethodPost, "/acme/_apis/contribution/hierarchyquery", `{}`), want: "denied_internal"},
	{name: "unknown area GET", req: adoReq(http.MethodGet, "/acme/_apis/frobnicate", ""), want: "unclassified_read"},
	{name: "unknown area POST", req: adoReq(http.MethodPost, "/acme/_apis/frobnicate", `{}`), want: "unclassified_write"},
}

// TestClassifyPerArea is the golden: every row of perAreaCases classifies to
// exactly its capability, or is refused.
func TestClassifyPerArea(t *testing.T) {
	for _, tc := range perAreaCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Classify(tc.req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Classify() = %q, want a refusal", got.Capability)
				}
				return
			}
			if err != nil {
				t.Fatalf("Classify() error = %v, want %q", err, tc.want)
			}
			if got.Capability != tc.want {
				t.Fatalf("Classify() = %q, want %q", got.Capability, tc.want)
			}
		})
	}
	// Every deniedAreas key keeps its refusal, for reads and writes alike.
	for area, want := range deniedAreas {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			got, err := Classify(adoReq(m, "/acme/_apis/"+area+"/x", `{}`))
			if err != nil || got.Capability != want {
				t.Errorf("Classify(%s %s) = %q, %v — want %q", m, area, got.Capability, err, want)
			}
		}
	}
}
