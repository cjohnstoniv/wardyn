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

// RegisterRepo makes barePath (a bare git repository — see NewFixtureRepo, or
// a caller-built one) reachable at /{org}/{project}/_git/{repo}, the real
// Azure DevOps git smart-HTTP URL shape. It sets http.receivepack=true on
// barePath: git http-backend refuses receive-pack for any repo that hasn't
// opted in, regardless of GIT_HTTP_EXPORT_ALL, so a caller-supplied repo that
// skipped this would be granted ScopeCodeWrite by the fake and then die 403
// inside git for an unrelated config reason. Panics (loudly, at registration
// time rather than at the first confusing push) if barePath isn't a git
// directory git can configure.
func (s *Server) RegisterRepo(org, project, repo, barePath string) {
	if out, err := exec.Command("git", "-C", barePath, "config", "http.receivepack", "true").CombinedOutput(); err != nil {
		panic("adofake: RegisterRepo(" + barePath + "): git config http.receivepack: " + err.Error() + "\n" + string(out))
	}
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

// gitOperation classifies a git smart-HTTP request into the Endpoint/scope it
// needs. ok is false for anything real Azure DevOps' smart-HTTP-only surface
// 404s: the dumb protocol (bare .../HEAD, .../objects/..., an info/refs with
// no service), and an info/refs carrying more than one ?service= value.
//
// The more-than-one case is not a style nicety: net/http's
// url.Values.Get reads the FIRST value of a repeated query parameter, while
// git http-backend itself reads the LAST. A request built as
// "?service=git-upload-pack&service=git-receive-pack" would therefore be
// scope-checked here as a read (Get sees "git-upload-pack") and then handed
// to http-backend, which would serve a REAL receive-pack advertisement (the
// value it reads is "git-receive-pack") to a read-only token. Refusing
// outright when more than one value is present closes that gap regardless of
// which value either side would have picked.
func gitOperation(r *http.Request, restPath string) (endpoint Endpoint, scope string, ok bool) {
	switch restPath {
	case "info/refs":
		services := r.URL.Query()["service"]
		switch {
		case len(services) != 1:
			return "", "", false
		case services[0] == "git-receive-pack":
			return EndpointGitAdvertise, ScopeCodeWrite, true
		case services[0] == "git-upload-pack":
			return EndpointGitAdvertise, ScopeCodeRead, true
		default:
			return "", "", false
		}
	case "git-upload-pack":
		return EndpointGitUploadPack, ScopeCodeRead, true
	case "git-receive-pack":
		return EndpointGitReceivePack, ScopeCodeWrite, true
	default:
		return "", "", false
	}
}

// handleGit serves real git smart-HTTP for a registered repository via `git
// http-backend` (net/http/cgi), so a clone or a push is driven against an
// ACTUAL git implementation rather than a reimplementation of the protocol.
func (s *Server) handleGit(w http.ResponseWriter, r *http.Request) {
	org, project, repo := r.PathValue("org"), r.PathValue("project"), r.PathValue("repo")
	rest := r.PathValue("path")

	endpoint, scope, ok := gitOperation(r, rest)
	if !ok {
		// Real Azure DevOps is smart-HTTP only; a fake that answered 200 to
		// the dumb protocol or an ambiguous advertisement request would let a
		// lane "prove" a read through a path production can never use.
		http.NotFound(w, r)
		return
	}

	// The scope decision is always computed and recorded, even under a
	// SetOverride — a lane asserting on Requests() must see what WOULD have
	// happened, not a decision that never ran.
	token, granted := s.checkScope(r, scope)
	s.record(endpoint, scope, r, token, granted)

	s.mu.Lock()
	ov, overridden := s.overrides[endpoint]
	s.mu.Unlock()
	if overridden {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ov.status)
		_, _ = w.Write(ov.body)
		return
	}
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
