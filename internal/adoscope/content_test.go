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
	}{
		{"a push", http.MethodPost, repo + "pushes", `{}`, ContentPush},
		{"a push under a mixed-case route", http.MethodPost, "/acme/proj/_apis/Git/Repositories/app/Pushes", `{}`, ContentPush},
		{"listing pushes", http.MethodGet, repo + "pushes", ``, NoContentWrite},
		{"an import", http.MethodPost, repo + "importrequests", `{}`, ContentOpaque},
		{"a server-side cherry-pick", http.MethodPost, repo + "cherrypicks", `{}`, ContentOpaque},
		{"a merge", http.MethodPost, repo + "merges", `{}`, ContentOpaque},
		{"a fork sync", http.MethodPost, repo + "forksyncrequests", `{}`, ContentOpaque},
		{"an annotated tag", http.MethodPost, repo + "annotatedtags", `{}`, ContentOpaque},
		{"a ref pointed at a commit", http.MethodPost, repo + "refs",
			`[{"name":"refs/heads/x","newObjectId":"` + "1111111111111111111111111111111111111111" + `"}]`, ContentOpaque},
		{"a ref deleted", http.MethodPost, repo + "refs",
			`[{"name":"refs/heads/x","newObjectId":"0000000000000000000000000000000000000000"}]`, NoContentWrite},
		{"a pull request completed", http.MethodPatch, repo + "pullrequests/7", `{"status":"completed"}`, ContentOpaque},
		{"a pull request set to auto-complete", http.MethodPatch, repo + "pullrequests/7", `{"autoCompleteSetBy":{"id":"x"}}`, ContentOpaque},
		{"a pull request retitled", http.MethodPatch, repo + "pullrequests/7", `{"title":"x"}`, NoContentWrite},
		{"a pull request created", http.MethodPost, repo + "pullrequests", `{"title":"x"}`, NoContentWrite},
		{"a wiki page", http.MethodPut, "/acme/proj/_apis/wiki/wikis/w/pages", `{}`, ContentOpaque},
		{"a TFVC check-in", http.MethodPost, "/acme/proj/_apis/tfvc/changesets", `{}`, ContentOpaque},
		{"a work item", http.MethodPatch, "/acme/proj/_apis/wit/workitems/1", `[]`, NoContentWrite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ClassifyContent(contentReq(c.method, c.path, c.body))
			if err != nil || got.Write != c.want {
				t.Fatalf("ClassifyContent = %+v, %v; want %v", got, err, c.want)
			}
		})
	}
	if got, _ := ClassifyContent(contentReq(http.MethodPost, repo+"pushes", `{}`)); got.RepoPath != "proj/_git/app" {
		t.Errorf("RepoPath = %q, want proj/_git/app", got.RepoPath)
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
