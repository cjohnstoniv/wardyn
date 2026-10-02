// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// TestReadSecretValue covers the multi-line secret truncation finding: reading a
// secret value from stdin must preserve the ENTIRE input (e.g. a PEM private
// key or a JSON blob spanning many lines), not just the first line. The old
// implementation used bufio.ReadString('\n') and silently truncated everything
// after the first newline.
func TestReadSecretValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single line trailing newline stripped",
			in:   "hunter2\n",
			want: "hunter2",
		},
		{
			name: "single line crlf stripped",
			in:   "hunter2\r\n",
			want: "hunter2",
		},
		{
			name: "single line no trailing newline",
			in:   "hunter2",
			want: "hunter2",
		},
		{
			name: "multi-line preserves internal newlines",
			in:   "-----BEGIN KEY-----\nline2\nline3\n-----END KEY-----\n",
			want: "-----BEGIN KEY-----\nline2\nline3\n-----END KEY-----",
		},
		{
			name: "multi-line no trailing newline",
			in:   "line1\nline2\nline3",
			want: "line1\nline2\nline3",
		},
		{
			name: "only a single trailing newline removed, blank lines kept",
			in:   "line1\n\nline3\n\n",
			want: "line1\n\nline3\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readSecretValue(strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("readSecretValue(%q) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("readSecretValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// operatorCommand runs the real root command against server with stdin, and
// returns stdout and the command error. Shared by the operator-path tests
// (redirects, trailing documents, wait deadlines) in this package.
func operatorCommand(t *testing.T, server, input string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("WARDYN_ADMIN_TOKEN", "")
	t.Setenv("WARDYN_TOKEN", "")
	c := rootCmd()
	var stdout, stderr bytes.Buffer
	c.SetOut(&stdout)
	c.SetErr(&stderr)
	c.SetIn(strings.NewReader(input))
	c.SetArgs(append([]string{"--url", server, "--token", "tok"}, args...))
	err := c.Execute()
	return stdout.String(), err
}

// redirectTarget is a second server standing in for wherever a hostile or
// misconfigured redirect points. Every request it sees is recorded: the point
// of the redirect tests is that it sees none.
type redirectTarget struct {
	*httptest.Server
	mu   sync.Mutex
	hits []string
}

func newRedirectTarget(t *testing.T) *redirectTarget {
	t.Helper()
	rt := &redirectTarget{}
	rt.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rt.mu.Lock()
		rt.hits = append(rt.hits, fmt.Sprintf("%s %s auth=%q body=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"), body))
		rt.mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>Sign in first</html>")
	}))
	t.Cleanup(rt.Close)
	return rt
}

func (rt *redirectTarget) seen() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.hits...)
}

// redirectingServer answers every request with status and a Location to loc.
func redirectingServer(t *testing.T, status int, loc string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, loc, status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestOperatorRedirectWritesClaimSuccess is #1490's reproduction, flipped: a
// 302 with a Location answering a secret PUT used to be followed (the PUT
// became a GET of the login page, which answered 200) and the CLI printed
// `secret "demo" stored` for a write the control plane never saw. Now the 302
// is the answer: exit 3, no success line, and the Location never visited.
func TestOperatorRedirectWritesClaimSuccess(t *testing.T) {
	target := newRedirectTarget(t)
	srv := redirectingServer(t, http.StatusFound, target.URL+"/login")
	out, err := operatorCommand(t, srv.URL, "synthetic-secret\n", "secret", "set", "demo")
	if err == nil {
		t.Fatalf("a redirected write returned success: out=%q", out)
	}
	if strings.Contains(out, "stored") {
		t.Errorf("a success line was printed for a write that never happened: %q", out)
	}
	if code := exitCodeFor(err); code != 3 {
		t.Errorf("exit code = %d, want 3 (err=%v)", code, err)
	}
	if hits := target.seen(); len(hits) != 0 {
		t.Errorf("the redirect target was visited: %q", hits)
	}
}

// A 307/308 is the dangerous pair: Go replays the method AND the body, so a
// followed one carries the secret value to wherever Location says. None of
// them may leave the origin, to another host or to a name that resolves to
// the same one.
func TestOperatorRedirectNeverLeavesTheOrigin(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, via := range []string{"other host", "localhost alias"} {
			t.Run(fmt.Sprintf("%d/%s", status, via), func(t *testing.T) {
				target := newRedirectTarget(t)
				loc := target.URL + "/steal"
				if via == "localhost alias" {
					loc = strings.Replace(loc, "127.0.0.1", "localhost", 1)
				}
				srv := redirectingServer(t, status, loc)
				out, err := operatorCommand(t, srv.URL, "synthetic-secret\n", "secret", "set", "demo")
				if err == nil || strings.Contains(out, "stored") {
					t.Fatalf("redirected write succeeded: out=%q err=%v", out, err)
				}
				if code := exitCodeFor(err); code != 3 {
					t.Errorf("exit code = %d, want 3 (%v)", code, err)
				}
				if hits := target.seen(); len(hits) != 0 {
					t.Errorf("secret body or bearer left the origin: %q", hits)
				}
			})
		}
	}
}

// A redirect on a JSON read is the same failure on the read side.
func TestOperatorRedirectOnJSONReadFails(t *testing.T) {
	target := newRedirectTarget(t)
	srv := redirectingServer(t, http.StatusFound, target.URL+"/login")
	_, err := operatorCommand(t, srv.URL, "", "secret", "list")
	if err == nil {
		t.Fatal("a redirected JSON read succeeded")
	}
	if code := exitCodeFor(err); code != 3 {
		t.Errorf("exit code = %d, want 3 (%v)", code, err)
	}
	if hits := target.seen(); len(hits) != 0 {
		t.Errorf("the redirect target was visited: %q", hits)
	}
}

// A redirect on the streamed download writes no file: the 302 is an error
// before the first byte, not a recording to save.
func TestOperatorRedirectOnStreamWritesNoCast(t *testing.T) {
	target := newRedirectTarget(t)
	srv := redirectingServer(t, http.StatusFound, target.URL+"/login")
	out := filepath.Join(t.TempDir(), "run.cast")
	_, err := operatorCommand(t, srv.URL, "", "run", "recording", uuid.New().String(), "-o", out)
	if err == nil {
		t.Fatal("a redirected recording download succeeded")
	}
	if code := exitCodeFor(err); code != 3 {
		t.Errorf("exit code = %d, want 3 (%v)", code, err)
	}
	if _, serr := os.Stat(out); serr == nil {
		t.Errorf("a .cast was written from a redirect")
	}
	if hits := target.seen(); len(hits) != 0 {
		t.Errorf("the redirect target was visited: %q", hits)
	}
}

// Negative: a 2xx write is unchanged.
func TestOperatorSecretSetSucceedsOn2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	out, err := operatorCommand(t, srv.URL, "synthetic-secret\n", "secret", "set", "demo")
	if err != nil || !strings.Contains(out, `secret "demo" stored`) {
		t.Fatalf("out=%q err=%v, want the success line", out, err)
	}
}
