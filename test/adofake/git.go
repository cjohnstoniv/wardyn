// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adofake

import (
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// RegisterRepo makes barePath (a bare git repository — see NewFixtureRepo)
// reachable at /{org}/{project}/_git/{repo}, the real Azure DevOps git
// smart-HTTP URL shape.
func (s *Server) RegisterRepo(org, project, repo, barePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[org+"/"+project+"/"+repo] = barePath
}

var gitPathOnce struct {
	sync.Once
	path string
	err  error
}

func resolveGit() (string, error) {
	gitPathOnce.Do(func() {
		gitPathOnce.path, gitPathOnce.err = exec.LookPath("git")
	})
	return gitPathOnce.path, gitPathOnce.err
}

// handleGit serves real git smart-HTTP for a registered repository via `git
// http-backend` (net/http/cgi), so a clone or a push is driven against an
// ACTUAL git implementation rather than a reimplementation of the protocol.
// The advertisement (GET info/refs?service=...) requires read scope for
// upload-pack and write scope for receive-pack; POST git-receive-pack always
// requires write; everything else (POST git-upload-pack, dumb-protocol object
// fetches) requires read.
func (s *Server) handleGit(w http.ResponseWriter, r *http.Request) {
	org, project, repo := r.PathValue("org"), r.PathValue("project"), r.PathValue("repo")
	rest := r.PathValue("path")

	var endpoint Endpoint
	var scope string
	switch {
	case rest == "info/refs" && r.URL.Query().Get("service") == "git-receive-pack":
		endpoint, scope = EndpointGitAdvertise, ScopeCodeWrite
	case rest == "info/refs":
		endpoint, scope = EndpointGitAdvertise, ScopeCodeRead
	case rest == "git-receive-pack":
		endpoint, scope = EndpointGitReceivePack, ScopeCodeWrite
	default: // git-upload-pack, and any dumb-protocol object fetch
		endpoint, scope = EndpointGitUploadPack, ScopeCodeRead
	}

	s.mu.Lock()
	ov, overridden := s.overrides[endpoint]
	s.mu.Unlock()
	if overridden {
		s.record(endpoint, scope, r, tokenFromRequest(r), false)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ov.status)
		_, _ = w.Write(ov.body)
		return
	}

	token, granted := s.checkScope(r, scope)
	s.record(endpoint, scope, r, token, granted)
	if !granted {
		writeADOUnauthorized(w)
		return
	}

	s.mu.Lock()
	barePath, ok := s.repos[org+"/"+project+"/"+repo]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}

	gitBin, err := resolveGit()
	if err != nil {
		http.Error(w, "adofake: git not found on PATH: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// GIT_PROJECT_ROOT is the bare repo itself, so PATH_INFO is just the
	// service suffix (e.g. "/info/refs") — http-backend strips that suffix
	// from PATH_TRANSLATED and finds GIT_PROJECT_ROOT is already a valid git
	// directory. r2 carries the rewritten path; the original request (body,
	// headers, query) is otherwise untouched.
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + rest
	r2.URL.RawPath = ""
	(&cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Dir:  barePath,
		Env: []string{
			"GIT_PROJECT_ROOT=" + barePath,
			"GIT_HTTP_EXPORT_ALL=1",
		},
		InheritEnv: []string{"PATH"},
	}).ServeHTTP(w, r2)
}

// NewFixtureRepo creates a bare git repository with two commits on `main`,
// suitable for RegisterRepo — enough history that both a clone and a push (of
// a third commit) are meaningful. Skips the calling test with a clear message
// if `git` is not on PATH.
func NewFixtureRepo(tb testing.TB) string {
	tb.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		tb.Skip("adofake: git not found on PATH, skipping git-backed test")
	}

	bare := tb.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
		tb.Fatalf("git init --bare: %v\n%s", err, out)
	}
	// http-backend refuses receive-pack unless the TARGET repo opts in — unlike
	// upload-pack, GIT_HTTP_EXPORT_ALL alone does not enable it. Scope
	// enforcement (ScopeCodeWrite) is the fake's own gate on push; this just
	// lets git http-backend itself get that far.
	if out, err := exec.Command("git", "-C", bare, "config", "http.receivepack", "true").CombinedOutput(); err != nil {
		tb.Fatalf("git config http.receivepack: %v\n%s", err, out)
	}

	work := tb.TempDir()
	runGit := func(args ...string) {
		tb.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=adofake", "GIT_AUTHOR_EMAIL=adofake@example.com",
			"GIT_COMMITTER_NAME=adofake", "GIT_COMMITTER_EMAIL=adofake@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			tb.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	readme := filepath.Join(work, "README.md")
	if err := os.WriteFile(readme, []byte("adofake fixture\n"), 0o644); err != nil {
		tb.Fatalf("write README.md: %v", err)
	}
	runGit("add", "README.md")
	runGit("commit", "-m", "first commit")
	if err := os.WriteFile(readme, []byte("adofake fixture\nsecond line\n"), 0o644); err != nil {
		tb.Fatalf("rewrite README.md: %v", err)
	}
	runGit("commit", "-am", "second commit")
	runGit("remote", "add", "origin", bare)
	runGit("push", "origin", "main")

	return bare
}
