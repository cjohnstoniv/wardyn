// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"errors"
	"net/http"
	"slices"
	"testing"
)

func contentReq(method, path, body string) Request {
	return Request{Method: method, Host: "dev.azure.com", Path: path, Org: "acme", BodyPeek: []byte(body)}
}

// TestClassifyContent pins which REST routes write repository content, and
// that the answer uses the classifier's own route reading.
func TestClassifyContent(t *testing.T) {
	const repo = "/acme/proj/_apis/git/repositories/app/"
	cases := []struct {
		name, method, path, body string
		want                     ContentWrite
		override                 string // X-HTTP-Method-Override
	}{
		{"a push", http.MethodPost, repo + "pushes", `{}`, ContentPush, ""},
		{"a push under a mixed-case route", http.MethodPost, "/acme/proj/_apis/Git/Repositories/app/Pushes", `{}`, ContentPush, ""},
		{"listing pushes", http.MethodGet, repo + "pushes", ``, NoContentWrite, ""},
		{"an import", http.MethodPost, repo + "importrequests", `{}`, ContentOpaque, ""},
		{"a server-side cherry-pick", http.MethodPost, repo + "cherrypicks", `{}`, ContentOpaque, ""},
		{"a merge", http.MethodPost, repo + "merges", `{}`, ContentOpaque, ""},
		{"a fork sync", http.MethodPost, repo + "forksyncrequests", `{}`, ContentOpaque, ""},
		{"an annotated tag", http.MethodPost, repo + "annotatedtags", `{}`, ContentOpaque, ""},
		{"a ref pointed at a commit", http.MethodPost, repo + "refs",
			`[{"name":"refs/heads/x","newObjectId":"` + "1111111111111111111111111111111111111111" + `"}]`, ContentOpaque, ""},
		{"a ref deleted", http.MethodPost, repo + "refs",
			`[{"name":"refs/heads/x","newObjectId":"0000000000000000000000000000000000000000"}]`, NoContentWrite, ""},
		{"a pull request completed", http.MethodPatch, repo + "pullrequests/7", `{"status":"completed"}`, ContentOpaque, ""},
		{"a pull request set to auto-complete", http.MethodPatch, repo + "pullrequests/7", `{"autoCompleteSetBy":{"id":"x"}}`, ContentOpaque, ""},
		{"a pull request retitled", http.MethodPatch, repo + "pullrequests/7", `{"title":"x"}`, NoContentWrite, ""},
		{"a pull request created", http.MethodPost, repo + "pullrequests", `{"title":"x"}`, NoContentWrite, ""},
		{"a wiki page", http.MethodPut, "/acme/proj/_apis/wiki/wikis/w/pages", `{}`, ContentOpaque, ""},
		{"a TFVC check-in", http.MethodPost, "/acme/proj/_apis/tfvc/changesets", `{}`, ContentOpaque, ""},
		{"a work item", http.MethodPatch, "/acme/proj/_apis/wit/workitems/1", `[]`, NoContentWrite, ""},
		// The EFFECTIVE method decides, and on a content resource any write
		// verb is content: a push body smuggled under an override is opaque,
		// never a non-write.
		{"a push overridden to PUT", http.MethodPost, repo + "pushes", `{}`, ContentOpaque, http.MethodPut},
		{"a push overridden to PATCH", http.MethodPost, repo + "pushes", `{}`, ContentOpaque, http.MethodPatch},
		{"a push overridden to DELETE", http.MethodPost, repo + "pushes", `{}`, ContentOpaque, http.MethodDelete},
		{"a push sent as PUT", http.MethodPut, repo + "pushes", `{}`, ContentOpaque, ""},
		{"an import overridden to PUT", http.MethodPost, repo + "importrequests", `{}`, ContentOpaque, http.MethodPut},
		{"a merge overridden to PATCH", http.MethodPost, repo + "merges", `{}`, ContentOpaque, http.MethodPatch},
		// A pull request created already set to complete itself.
		{"a pull request created with auto-complete", http.MethodPost, repo + "pullrequests",
			`{"title":"x","autoCompleteSetBy":{"id":"x"}}`, ContentOpaque, ""},
		{"a pull request created completed", http.MethodPost, repo + "pullrequests",
			`{"title":"x","status":"completed"}`, ContentOpaque, ""},
		{"a project-scoped pull request completed", http.MethodPatch, "/acme/proj/_apis/git/pullrequests/7",
			`{"status":"completed"}`, ContentOpaque, ""},
		// Content-free actions on a pull request's parts: no body is read.
		{"a reviewer vote", http.MethodPut, repo + "pullrequests/7/reviewers/r", `{"vote":10}`, NoContentWrite, ""},
		{"a pull request status", http.MethodPost, repo + "pullrequests/7/statuses", `{"state":"succeeded"}`, NoContentWrite, ""},
		{"a comment thread", http.MethodPost, repo + "pullrequests/7/threads", `{"status":"active"}`, NoContentWrite, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := contentReq(c.method, c.path, c.body)
			if c.override != "" {
				req.Header = http.Header{"X-Http-Method-Override": {c.override}}
			}
			got, err := ClassifyContent(req)
			if err != nil || got.Write != c.want {
				t.Fatalf("ClassifyContent = %+v, %v; want %v", got, err, c.want)
			}
		})
	}
	if got, _ := ClassifyContent(contentReq(http.MethodPost, repo+"pushes", `{}`)); got.RepoPath != "proj/_git/app" {
		t.Errorf("RepoPath = %q, want proj/_git/app", got.RepoPath)
	}
	lowered := contentReq(http.MethodPost, repo+"pushes", `{}`)
	lowered.Header = http.Header{"X-Http-Method-Override": {http.MethodGet}}
	if _, err := ClassifyContent(lowered); err == nil {
		t.Error("a push overridden down to GET was classified, want an error (the gate refuses it)")
	}
	withheld := contentReq(http.MethodPost, repo+"refs", "")
	withheld.BodyWithheld = true
	if _, err := ClassifyContent(withheld); !errors.Is(err, ErrNeedsBody) {
		t.Errorf("a withheld ref-update body = %v, want ErrNeedsBody", err)
	}
	if _, err := ClassifyContent(contentReq(http.MethodPatch, repo+"pullrequests/7", `{"status":3}`)); err == nil {
		t.Error("a pull-request status the reader cannot type was classified, want an error")
	}
}

// TestParsePush reads a Git Pushes - Create body path by path, and refuses one
// it cannot read whole.
func TestParsePush(t *testing.T) {
	body := `{"refUpdates":[{"name":"refs/heads/a","oldObjectId":"0"}],"commits":[{"changes":[` +
		`{"changeType":"add","item":{"path":"/.github/workflows/x.yml"},"newContent":{"content":"x"}},` +
		`{"changeType":"rename","sourceServerItem":"/stage","item":{"path":"/infra/"}}]}]}`
	p, err := ParsePush(contentReq(http.MethodPost, "", body))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Refs, []string{"refs/heads/a"}) || !slices.Equal(p.Paths, []string{".github/workflows/x.yml", "infra", "stage"}) || len(p.Digest) != 64 {
		t.Errorf("ParsePush = %+v", p)
	}
	for name, bad := range map[string]string{
		"no ref":         `{"refUpdates":[],"commits":[]}`,
		"no item":        `{"refUpdates":[{"name":"refs/heads/a"}],"commits":[{"changes":[{"changeType":"add"}]}]}`,
		"a .. segment":   `{"refUpdates":[{"name":"refs/heads/a"}],"commits":[{"changes":[{"item":{"path":"/a/../b"}}]}]}`,
		"a backslash":    `{"refUpdates":[{"name":"refs/heads/a"}],"commits":[{"changes":[{"item":{"path":"/a\\b"}}]}]}`,
		"the root":       `{"refUpdates":[{"name":"refs/heads/a"}],"commits":[{"changes":[{"item":{"path":"/"}}]}]}`,
		"a repeated key": `{"refUpdates":[{"name":"refs/heads/a"}],"commits":[],"COMMITS":[]}`,
		"not JSON":       `{"refUpdates":`,
	} {
		if _, err := ParsePush(contentReq(http.MethodPost, "", bad)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
}
