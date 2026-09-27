// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// BODY WITHHELD. A gate classifies on the path first and peeks only when
// Classify says the route reads the body; every other body streams through
// untouched. The routes that read it are exactly the three body routes (and
// OPTIONS, which must prove it carries none).
func TestClassifyWithheldBodyAsksOnlyWhereTheBodyDecides(t *testing.T) {
	withheld := func(method, host, path string) Request {
		return Request{Method: method, Host: host, Path: path, Org: "acme", BodyWithheld: true,
			Header: hdr("Content-Length", "1048576")}
	}
	needsBody := []Request{
		withheld(http.MethodPatch, "dev.azure.com", "/acme/proj/_apis/git/repositories/app/pullrequests/5"),
		withheld(http.MethodPatch, "dev.azure.com", "/acme/proj/_apis/git/pullrequests/5"),
		withheld(http.MethodPost, "dev.azure.com", "/acme/proj/_apis/git/repositories/app/refs"),
		withheld(http.MethodPost, "dev.azure.com", "/acme/proj/_apis/git/repositories/app/pushes"),
		withheld(http.MethodPost, "dev.azure.com", "/acme/_apis/wit/$batch"),
		withheld(http.MethodOptions, "dev.azure.com", "/acme/_apis"),
	}
	for _, req := range needsBody {
		if _, err := Classify(req); !errors.Is(err, ErrNeedsBody) {
			t.Errorf("%s %s withheld: err = %v, want ErrNeedsBody", req.Method, req.Path, err)
		}
	}
	for _, tc := range []struct {
		req  Request
		want Capability
	}{
		{withheld(http.MethodPut, "dev.azure.com", "/acme/proj/_apis/wiki/wikis/w/attachments"), CapWikiWrite},
		{withheld(http.MethodPut, "pkgs.dev.azure.com", "/acme/_packaging/feed/npm/registry/pkg"), CapPackagingWrite},
		{withheld(http.MethodPatch, "dev.azure.com", "/acme/proj/_apis/wit/workitems/1"), CapWorkWrite},
		{withheld(http.MethodDelete, "dev.azure.com", "/acme/proj/_apis/git/repositories/app/pullrequests/5"), CapPR},
		{withheld(http.MethodGet, "dev.azure.com", "/acme/_apis/projects"), CapRead},
	} {
		got, err := Classify(tc.req)
		if err != nil || got.Capability != tc.want {
			t.Errorf("%s %s withheld = %q, %v; want %q classified on the path alone", tc.req.Method, tc.req.Path, got.Capability, err, tc.want)
		}
	}
}

// ONE REF-NAME RULE ACROSS BOTH DOORS. The REST refs/pushes body is held to
// the same rule the git broker's receive-pack parser applies (CheckRefName),
// so a traversal inside the run namespace cannot read as a run-namespace move.
func TestRESTRefNamesAreHeldToTheGitRule(t *testing.T) {
	const pushes = "/acme/proj/_apis/git/repositories/app/pushes"
	const refs = "/acme/proj/_apis/git/repositories/app/refs"
	runNS := func(ref string) bool { return !strings.HasPrefix(ref, "refs/heads/wardyn/RUN/") }
	onPush := func(name string) Request {
		r := adoReq(http.MethodPost, pushes, `{"refUpdates":[{"name":"`+name+`"}]}`)
		r.RefProtected = runNS
		return r
	}
	runCases(t, []caseT{
		{name: "a run-namespace ref", req: onPush("refs/heads/wardyn/RUN/work"), want: CapCodeWrite, wantRefs: []string{"refs/heads/wardyn/RUN/work"}},
		{name: "a traversal out of the run namespace", req: onPush("refs/heads/wardyn/RUN/../../main"), wantErr: true},
		{name: "a caret", req: onPush("refs/heads/wardyn/RUN/a^b"), wantErr: true},
		{name: "a tilde", req: onPush("refs/heads/wardyn/RUN/a~1"), wantErr: true},
		{name: "a colon", req: onPush("refs/heads/wardyn/RUN/a:refs/heads/main"), wantErr: true},
		{name: "a glob", req: onPush("refs/heads/wardyn/RUN/*"), wantErr: true},
		{name: "a bracket", req: onPush("refs/heads/wardyn/RUN/a[b"), wantErr: true},
		{name: "a question mark", req: onPush("refs/heads/wardyn/RUN/a?"), wantErr: true},
		{name: "an inner space", req: onPush("refs/heads/wardyn/RUN/a refs/heads/main"), wantErr: true},
		{name: "a tab", req: onPush(`refs/heads/wardyn/RUN/a\tb`), wantErr: true},
		{name: "a newline", req: onPush(`refs/heads/wardyn/RUN/a\nrefs/heads/main`), wantErr: true},
		{name: "a DEL byte", req: onPush(`refs/heads/wardyn/RUN/a\u007f`), wantErr: true},
		{name: "a traversal on the refs door", req: adoReq(http.MethodPost, refs, `[{"name":"refs/heads/wardyn/RUN/../../main"}]`), wantErr: true},
	})
}

func TestCheckRefName(t *testing.T) {
	for _, ok := range []string{"refs/heads/main", "refs/heads/wardyn/run-1/feature", "refs/tags/v1.2.3", "refs/heads/a.b"} {
		if err := CheckRefName(ok); err != nil {
			t.Errorf("CheckRefName(%q) = %v, want accepted", ok, err)
		}
	}
	for _, bad := range []string{"refs/heads/../main", "refs/heads/a..b", `refs\heads\main`, "refs/heads/a b", "refs/heads/a\tb",
		"refs/heads/a^", "refs/heads/a~", "refs/heads/a:b", "refs/heads/a?", "refs/heads/a*", "refs/heads/a[", "refs/heads/a\nb", "refs/heads/a\x00", "refs/heads/a\x7f"} {
		if err := CheckRefName(bad); err == nil {
			t.Errorf("CheckRefName(%q) accepted, want refused", bad)
		}
	}
}
