// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/testutil"
)

// filesFake is a stand-in api.github.com for ReadRootFiles: a root of entries, each with a type, a size and a
// body, and a record of the token request, the refs the reads named and the hand-back.
type filesFake struct {
	status  int // non-zero: every repository read answers it
	entries map[string][3]string

	mu      sync.Mutex
	perms   string
	revoked bool
	refs    []string
	reads   []string
}

// entry is a root entry: its type, its size as the listing reports it and its body.
func entry(kind string, size int, body string) [3]string {
	return [3]string{kind, fmt.Sprint(size), body}
}

func (f *filesFake) serve(t *testing.T) *githubMinter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/installation/token"):
			f.revoked = true
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/installation"):
			_, _ = w.Write([]byte(`{"id": 42}`))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			b, _ := io.ReadAll(r.Body)
			f.perms = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_files","expires_at":"` + testutil.FutureRFC3339(24) + `"}`))
		case f.status != 0:
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"message":"refused"}`))
		case strings.HasSuffix(r.URL.Path, "/repos/acme/widgets"):
			_, _ = w.Write([]byte(`{"name":"widgets","default_branch":"trunk"}`))
		case strings.HasSuffix(r.URL.Path, "/repos/acme/widgets/contents/") || strings.HasSuffix(r.URL.Path, "/repos/acme/widgets/contents"):
			f.refs = append(f.refs, r.URL.Query().Get("ref"))
			var items []string
			for name, e := range f.entries {
				items = append(items, fmt.Sprintf(`{"type":%q,"name":%q,"path":%q,"size":%s}`, e[0], name, name, e[1]))
			}
			slices.Sort(items)
			_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
		case strings.Contains(r.URL.Path, "/repos/acme/widgets/contents/"):
			name := r.URL.Path[strings.Index(r.URL.Path, "/contents/")+len("/contents/"):]
			f.reads = append(f.reads, name)
			f.refs = append(f.refs, r.URL.Query().Get("ref"))
			e, ok := f.entries[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(fmt.Sprintf(`{"type":%q,"name":%q,"path":%q,"size":%d,"encoding":"base64","content":%q}`,
				e[0], name, name, len(e[2]), base64.StdEncoding.EncodeToString([]byte(e[2])))))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return newMinterAgainst(t, srv.URL+"/")
}

// The token is one repository, contents:read and nothing more, it is the one the reads use, and it is handed
// back. Only entries of type file with a listed name are read, in the order the names come, at the default
// branch; a symlink, a submodule, a directory and a name not asked for are left alone; a file over the cap is
// named without its text.
func TestReadRootFiles(t *testing.T) {
	f := &filesFake{entries: map[string][3]string{
		"package.json": entry("file", 15, `{"name":"demo"}`+"\n"),
		"Dockerfile":   entry("file", 13, "FROM node:20\n"),
		"go.sum":       entry("file", 100, strings.Repeat("x", 100)),
		"go.mod":       entry("symlink", 5, "x"),
		"Cargo.toml":   entry("submodule", 0, ""),
		"yarn.lock":    entry("dir", 0, ""),
		"README.md":    entry("file", 3, "hey"),
		"Gemfile":      entry("file", 3, "hey"),
	}}
	m := f.serve(t)
	files, err := m.ReadRootFiles(context.Background(), "acme/widgets", []string{"Dockerfile", "package.json", "go.mod", "Cargo.toml", "yarn.lock", "go.sum", "uv.lock"}, 50)
	if err != nil {
		t.Fatalf("ReadRootFiles: %v", err)
	}
	var got []string
	for _, x := range files {
		got = append(got, fmt.Sprintf("%s|%s|%v|%q", x.Name, x.Branch, x.TooLarge, x.Text))
	}
	want := []string{`Dockerfile|trunk|false|"FROM node:20\n"`, `package.json|trunk|false|"{\"name\":\"demo\"}\n"`, `go.sum|trunk|true|""`}
	if !slices.Equal(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
	if !strings.Contains(f.perms, `"permissions":{"contents":"read"}`) || !strings.Contains(f.perms, `"repositories":["widgets"]`) {
		t.Errorf("token request = %s, want one repository with contents:read only", f.perms)
	}
	if !f.revoked {
		t.Error("the read token was not handed back")
	}
	if !slices.Equal(f.reads, []string{"Dockerfile", "package.json"}) {
		t.Errorf("file reads = %v, want the two files that fit", f.reads)
	}
	for _, ref := range f.refs {
		if ref != "trunk" {
			t.Errorf("a read named ref %q, want the default branch", ref)
		}
	}
}

// A GitHub refusal is ErrGitHubRefused, as the branch list's is, and the token is still handed back.
func TestReadRootFilesRefusalIsErrGitHubRefused(t *testing.T) {
	f := &filesFake{status: http.StatusNotFound}
	m := f.serve(t)
	if _, err := m.ReadRootFiles(context.Background(), "acme/widgets", []string{"Dockerfile"}, 10); !errors.Is(err, ErrGitHubRefused) {
		t.Fatalf("err = %v, want ErrGitHubRefused", err)
	}
	if !f.revoked {
		t.Error("the read token was not handed back after a refusal")
	}
}

// The fake keeps the real read's contract: listed names only, in order, with the byte cap.
func TestReadRootFilesFake(t *testing.T) {
	fk := &FakeGitHubMinter{RootFiles: []File{{Name: "Dockerfile", Text: "FROM x"}, {Name: "package.json", Text: strings.Repeat("a", 20)}, {Name: ".npmrc", Text: "tok"}}}
	got, err := fk.ReadRootFiles(context.Background(), "acme/widgets", []string{"package.json", "Dockerfile"}, 10)
	if err != nil || len(got) != 2 || got[0].Name != "package.json" || !got[0].TooLarge || got[0].Text != "" || got[1].Name != "Dockerfile" || got[1].Text != "FROM x" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if !slices.Equal(fk.LastReadRepos, []string{"acme/widgets"}) {
		t.Errorf("reads = %v", fk.LastReadRepos)
	}
}
