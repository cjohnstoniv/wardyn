// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ghscope

import (
	"net/http"
	"testing"
)

// Requests no capability could be honest about: Classify errors, and an error
// carries no capability, so a caller that forgot to check err still holds
// nothing grantable.
func TestClassifyRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"another host", Request{Method: "GET", Host: "uploads.github.com", Path: in}},
		{"an enterprise server", Request{Method: "GET", Host: "ghe.example.com", Path: "/api/v3" + in}},
		{"raw content", Request{Method: "GET", Host: "raw.githubusercontent.com", Path: "/acme/app/main/x"}},
		{"a look-alike host", Request{Method: "GET", Host: "api.github.com.evil.example", Path: in}},
		{"no host", Request{Method: "GET", Path: in}},
		{"OPTIONS", Request{Method: "OPTIONS", Host: APIHost, Path: in}},
		{"TRACE", Request{Method: "TRACE", Host: APIHost, Path: in}},
		{"CONNECT", Request{Method: "CONNECT", Host: APIHost, Path: in}},
		{"an empty method", Request{Host: APIHost, Path: in}},
		{"a method override", Request{Method: "POST", Host: APIHost, Path: in + "/issues",
			Header: http.Header{"X-Http-Method-Override": {"DELETE"}}}},
		{"a method override spelled in lower case", Request{Method: "GET", Host: APIHost, Path: in,
			Header: http.Header{"x-http-method-override": {"GET"}}}},
		{"X-HTTP-Method", Request{Method: "POST", Host: APIHost, Path: in + "/issues",
			Header: http.Header{"X-Http-Method": {"DELETE"}}}},
		{"X-Method-Override", Request{Method: "POST", Host: APIHost, Path: in + "/issues",
			Header: http.Header{"X-Method-Override": {"PUT"}}}},
		{"X-Method-Override in lower case", Request{Method: "GET", Host: APIHost, Path: in,
			Header: http.Header{"x-method-override": {"DELETE"}}}},
		{"a relative path", rest("GET repos/acme/app")},
		{"an empty path", rest("GET ")},
		{"a query in the path", rest("GET " + in + "?x=1")},
		{"a fragment in the path", rest("GET " + in + "#x")},
		{"a doubled slash", rest("GET /repos//acme/app")},
		{"a doubled slash at the end", rest("GET " + in + "//")},
		{"a dot segment", rest("GET /repos/acme/app/../other")},
		{"a lone dot", rest("GET /repos/acme/./app")},
		{"an encoded dot segment", rest("GET /repos/acme/app/%2e%2e/other/pulls")},
		{"an encoded slash", rest("GET /repos/acme%2Fother/app")},
		{"an encoded slash in the area", rest("GET /repos/acme/app/hooks%2F1")},
		{"an encoded backslash", rest("GET /repos/acme/app%5Cx")},
		{"a backslash", rest(`GET /repos/acme/app\..\other`)},
		{"a double-encoded slash", rest("GET /repos/acme/app%252Fx")},
		{"a triple-encoded dot segment", rest("GET /repos/acme/app/%25252e%25252e")},
		{"a malformed escape", rest("GET /repos/acme/app%zz")},
		{"a truncated escape", rest("GET /repos/acme/app%2")},
		{"a control character", rest("GET /repos/acme/app%0a")},
		{"a NUL", rest("GET /repos/acme/app%00")},
		{"invalid UTF-8", rest("GET /repos/acme/app%ff")},
		{"an escaped owner", rest("GET /repos/%61cme/app")},
		{"an escaped repository", rest("GET /repos/acme/%61pp")},
		{"an owner with a dot", rest("GET /repos/ac.me/app")},
		{"an owner with a space", rest("GET /repos/ac%20me/app")},
		{"a repository named .git", rest("GET /repos/acme/.git")},
		{"a repository named .git twice", rest("GET /repos/acme/app.git.git")},
		{"a repository named ..", rest("GET /repos/acme/...git")},
		{"a non-ASCII repository", rest("GET /repos/acme/%C3%A4pp")},
		{"an organisation that is not a name", rest("GET /orgs/ac.me/members")},
		{"a package owner that is not a name", rest("GET /users/o.c/packages")},
		{"a write on a bad owner", rest("DELETE /repos/a%2fb/app")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Classify(tc.req)
			if err == nil {
				t.Fatalf("Classify() = %+v, want a refusal", got)
			}
			if got != (Verdict{}) {
				t.Fatalf("a refusal returned %+v — it must carry nothing", got)
			}
		})
	}
}

// Git over HTTP on github.com: a fetch is code_read and a push code_write, on
// the named repository, with or without ".git" and in any case.
func TestClassifyGitSmartHTTP(t *testing.T) {
	for _, tc := range []struct {
		method, path, query string
		want                Capability
		repo                string
	}{
		{"GET", "/acme/app.git/info/refs", "service=git-upload-pack", CapCodeRead, "acme/app"},
		{"GET", "/acme/app/info/refs", "service=git-upload-pack", CapCodeRead, "acme/app"},
		{"POST", "/acme/app.git/git-upload-pack", "", CapCodeRead, "acme/app"},
		{"GET", "/acme/app.git/info/refs", "service=git-receive-pack", CapCodeWrite, "acme/app"},
		{"POST", "/acme/app.git/git-receive-pack", "", CapCodeWrite, "acme/app"},
		{"POST", "/Acme/App.GIT/git-receive-pack", "", CapCodeWrite, "acme/app"},
		{"HEAD", "/acme/app.git/info/refs", "service=git-upload-pack", CapCodeRead, "acme/app"},
		{"GET", "/acme/app.git/info/refs/", "service=git-upload-pack", CapCodeRead, "acme/app"},
		{"GET", "/acme/app.git/HEAD", "", CapUnclassifiedRead, ""},
		{"GET", "/acme/app.git/objects/info/packs", "", CapUnclassifiedRead, ""},
		{"POST", "/acme/app.git/info/lfs/objects/batch", "", CapUnclassifiedWrite, ""},
		{"GET", "/acme/app", "", CapUnclassifiedRead, ""},
		{"GET", "/login/oauth/authorize", "", CapUnclassifiedRead, ""},
		{"POST", "/login/oauth/access_token", "", CapUnclassifiedWrite, ""},
	} {
		t.Run(tc.method+" "+tc.path+"?"+tc.query, func(t *testing.T) {
			got, err := Classify(Request{Method: tc.method, Host: GitHost, Path: tc.path, RawQuery: tc.query})
			if err != nil || got.Capability != tc.want || got.Repo != tc.repo {
				t.Fatalf("Classify() = %+v, %v, want %s on %q", got, err, tc.want, tc.repo)
			}
		})
	}
	for _, tc := range []struct{ method, path, query string }{
		{"GET", "/acme/app.git/info/refs", ""},
		{"GET", "/acme/app.git/info/refs", "service=git-upload-pack&service=git-receive-pack"},
		{"GET", "/acme/app.git/info/refs", "service=git-upload-archive"},
		{"GET", "/acme/app.git/info/refs", "x=1&service=git-upload-pack"},
		{"POST", "/acme/app.git/info/refs", "service=git-upload-pack"},
		{"PUT", "/acme/app.git/git-receive-pack", ""},
		{"GET", "/acme/app.git/git-upload-pack", ""},
		{"DELETE", "/acme/app.git/git-upload-pack", ""},
		{"POST", "/acme/%61pp.git/git-receive-pack", ""},
		{"POST", "/acme/app.git.git/git-receive-pack", ""},
		{"POST", "/ac.me/app.git/git-receive-pack", ""},
		{"POST", "/acme/app%2f..%2fother/git-receive-pack", ""},
	} {
		if got, err := Classify(Request{Method: tc.method, Host: GitHost, Path: tc.path, RawQuery: tc.query}); err == nil {
			t.Errorf("%s %s?%s = %+v, want a refusal", tc.method, tc.path, tc.query, got)
		}
	}
}

// The pin (G-4): a request is permitted only on the run's repositories, or on
// an organisation or account that owns one, and only with the capability.
func TestPermitsPinsToTheRunsRepositories(t *testing.T) {
	repos := []string{"Acme/App", "acme/lib.git"}
	codeRead := []Capability{CapCodeRead}
	for _, tc := range []struct {
		line    string
		granted []Capability
		want    bool
	}{
		{"GET /repos/acme/app/contents/f", codeRead, true},
		{"GET /repos/ACME/APP.git/contents/f", codeRead, true},
		{"GET /repos/acme/lib/pulls", codeRead, true},
		{"GET /repos/acme/other/contents/f", codeRead, false},
		{"GET /repos/b/y/contents/f", codeRead, false},
		{"GET /repos/acme/app", codeRead, true},
		{"GET /repos/b/y", codeRead, false},
		{"GET /repos/acme/app/topics", nil, false},
		{"GET /repos/acme/app/issues", codeRead, false},
		{"POST /repos/acme/app/issues", []Capability{CapIssuesWrite}, true},
		{"POST /repos/acme/app/pulls", []Capability{CapIssuesWrite}, false},
		{"POST /repos/acme/app/pulls", []Capability{CapPR}, true},
		{"POST /repos/b/y/pulls", []Capability{CapPR}, false},
		{"GET /orgs/acme/members", []Capability{CapOrgRead}, true},
		{"GET /orgs/ACME/members", []Capability{CapOrgRead}, true},
		{"GET /orgs/other/members", []Capability{CapOrgRead}, false},
		{"GET /orgs/acme/members", codeRead, false},
		{"GET /orgs/acme/packages", []Capability{CapPackagesRead}, true},
		{"GET /users/other/packages", []Capability{CapPackagesRead}, false},
		{"GET /user", codeRead, false},
		{"GET /user", []Capability{CapIdentity}, true},
		{"GET /user/repos", GrantableCapabilities(), false},
		{"GET /search/code", GrantableCapabilities(), false},
		{"POST /graphql", GrantableCapabilities(), false},
		{"GET /repositories/123", GrantableCapabilities(), false},
		{"GET /rate_limit", codeRead, true},
		{"GET /rate_limit", nil, false},
		{"GET /repos/acme/app/forks", GrantableCapabilities(), false},
		{"POST /repos/acme/app/forks", GrantableCapabilities(), false},
		{"PUT /repos/acme/app/contents/f", GrantableCapabilities(), false},
		{"PUT /repos/acme/app/pulls/5/merge", GrantableCapabilities(), false},
		{"GET /repos/acme/app/hooks", GrantableCapabilities(), false},
	} {
		v, err := Classify(rest(tc.line))
		if err != nil {
			t.Fatalf("%s: %v", tc.line, err)
		}
		if got := Permits(tc.granted, repos, v); got != tc.want {
			t.Errorf("%s with %v = %v, want %v (verdict %+v)", tc.line, tc.granted, got, tc.want, v)
		}
	}
	v, _ := Classify(rest("GET /repos/acme/app/pulls"))
	for name, rs := range map[string][]string{
		"no repositories": nil, "an unreadable entry": {"acme"}, "an escaped entry": {"acme/%61pp"},
		"a URL": {"https://github.com/acme/app"},
	} {
		if Permits(GrantableCapabilities(), rs, v) {
			t.Errorf("%s: a repository request is permitted", name)
		}
	}
	if !Permits([]Capability{CapPR, "bogus"}, nil, Verdict{Capability: CapMetadata}) {
		t.Error("a metadata read that names nothing is refused to a run holding a grantable capability")
	}
}

func TestRepoKey(t *testing.T) {
	for in, want := range map[string]string{
		"acme/app": "acme/app", " Acme/App.git ": "acme/app", "octo_corp/My.Repo": "octo_corp/my.repo",
		"acme/.github": "acme/.github",
	} {
		if got, ok := RepoKey(in); !ok || got != want {
			t.Errorf("RepoKey(%q) = %q, %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "acme", "acme/", "/app", "acme/app/x", "a.b/app", "acme/a b", "acme/app.git.git", "acme/..", "acme/%61pp"} {
		if got, ok := RepoKey(bad); ok {
			t.Errorf("RepoKey(%q) = %q, want refused", bad, got)
		}
	}
}
