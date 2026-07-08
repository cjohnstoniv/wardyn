// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuoteArgs(t *testing.T) {
	in := []string{"claude", "code", "fix the bug", "plain"}
	got := quoteArgs(in)
	if got[0] != "claude" || got[1] != "code" || got[3] != "plain" {
		t.Errorf("plain args must be unquoted, got %v", got)
	}
	if got[2] != "'fix the bug'" {
		t.Errorf("arg with spaces must be quoted, got %q", got[2])
	}
	// embedded single quote is escaped.
	if q := quoteArgs([]string{"a'b c"})[0]; !strings.Contains(q, `'\''`) {
		t.Errorf("single quote must be escaped, got %q", q)
	}
}

// TestBuildAsciinemaArgv_PreventsShellInjection is the regression test for the
// command-injection fix. The agent argv (untrusted run-task/command data) is
// joined into asciinema's `-c` string, which is evaluated by `$SHELL -c`. A
// crafted arg containing quotes, $(...), backticks, ;, |, &, redirections, or
// $VAR references MUST be treated as literal data — never executed or expanded.
//
// We drive the real -c string produced by buildAsciinemaArgv through a POSIX
// shell using `printf` as the recorded "program", then assert (a) no injected
// command ran (a sentinel file is never created) and (b) every payload reaches
// printf verbatim as a single argv element (no word-splitting/expansion).
func TestBuildAsciinemaArgv_PreventsShellInjection(t *testing.T) {
	pwned := filepath.Join(t.TempDir(), "pwned")

	payloads := []string{
		`"; touch ` + pwned + `; #`, // quote break-out + command chaining
		`$(touch ` + pwned + `)`,    // command substitution (no whitespace bypass)
		"`touch " + pwned + "`",     // backtick command substitution
		`'; touch ` + pwned + `; #`, // leading single quote break-out
		`x;rm -rf /;y`,              // separator injection, no surrounding space
		`a|b&c>d<e`,                 // pipes, background, redirections
		`$HOME${PATH}`,              // variable expansion
		`a'b`,                       // bare embedded single quote
	}

	// printf with the format "[%s]\n" reapplies the format once per operand,
	// echoing each payload on its own bracketed line iff it arrives literally.
	agentArgv := append([]string{"printf", "[%s]\n"}, payloads...)
	argv := buildAsciinemaArgv("/usr/bin/asciinema", filepath.Join(t.TempDir(), "out.cast"), agentArgv)

	// Pull out the exact -c command string wardyn-rec hands to asciinema.
	var cString string
	found := false
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-c" {
			cString = argv[i+1]
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no -c argument in built argv: %v", argv)
	}

	out, err := exec.Command("sh", "-c", cString).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -c failed: %v\noutput: %s\n-c string: %s", err, out, cString)
	}

	// (a) Nothing must have executed: the sentinel file must not exist.
	if _, statErr := os.Stat(pwned); statErr == nil {
		t.Fatalf("SHELL INJECTION: payload executed and created %s\n-c string: %s", pwned, cString)
	}

	// (b) Each payload must appear verbatim, proving it was one literal operand.
	got := string(out)
	for _, p := range payloads {
		want := "[" + p + "]"
		if !strings.Contains(got, want) {
			t.Errorf("payload not passed literally\n want substring: %q\n full output:\n%s\n -c string: %s", want, got, cString)
		}
	}
}

func TestFilenames(t *testing.T) {
	if got := castFile("/d", "abc"); got != filepath.Join("/d", "abc.cast") {
		t.Errorf("castFile = %q", got)
	}
	if got := logFile("/d", "abc"); got != filepath.Join("/d", "abc.log") {
		t.Errorf("logFile = %q", got)
	}
}

func TestRun_LogFallbackWhenNoAsciinema(t *testing.T) {
	dir := t.TempDir()
	// Point -asciinema at a binary that does not exist so LookPath fails and
	// we exercise the log-fallback path (which runs the agent argv directly).
	args := []string{
		"-cast-dir", dir,
		"-run", "run-xyz",
		"-asciinema", "definitely-not-a-real-binary-xyz",
		"--", "echo", "hello-wardyn",
	}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "run-xyz.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(b), "hello-wardyn") {
		t.Errorf("log must capture agent output, got %q", string(b))
	}
}

func TestRun_RequiresArgvAndRun(t *testing.T) {
	if err := run([]string{"-run", "x"}); err == nil {
		t.Error("missing argv must error")
	}
	if err := run([]string{"--", "echo", "hi"}); err == nil {
		t.Error("missing -run must error")
	}
}

// TestRun_OutDir verifies that -out-dir copies the log to the destination dir.
func TestRun_OutDir(t *testing.T) {
	castDir := t.TempDir()
	dstDir := t.TempDir()
	args := []string{
		"-cast-dir", castDir,
		"-run", "run-outdir",
		"-asciinema", "definitely-not-a-real-binary-xyz",
		"-out-dir", dstDir,
		"--", "echo", "wardyn-out-dir",
	}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The log file must exist in the destination dir.
	dst := filepath.Join(dstDir, "run-outdir.log")
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst log: %v", err)
	}
	if !strings.Contains(string(b), "wardyn-out-dir") {
		t.Errorf("dst log content = %q, want to contain 'wardyn-out-dir'", b)
	}
}

// TestRun_UploadURL verifies that -upload-url PUTs the log to the given server.
func TestRun_UploadURL(t *testing.T) {
	var received []byte
	var receivedToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "want PUT", http.StatusMethodNotAllowed)
			return
		}
		receivedToken = r.Header.Get("Authorization")
		var err error
		received, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	castDir := t.TempDir()
	args := []string{
		"-cast-dir", castDir,
		"-run", "run-upload",
		"-asciinema", "definitely-not-a-real-binary-xyz",
		"-upload-url", srv.URL + "/recording",
		"-run-token", "tok-abc",
		"--", "echo", "wardyn-upload",
	}
	if err := run(args); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(received), "wardyn-upload") {
		t.Errorf("uploaded body = %q, want to contain 'wardyn-upload'", received)
	}
	if receivedToken != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want 'Bearer tok-abc'", receivedToken)
	}
}

// TestRun_UploadURL_ServerError is the regression guard for the
// exit-code-on-success bug: a failed recording UPLOAD must be NON-FATAL. The
// agent already ran, so wardyn-rec must NOT turn a delivery error into a
// non-zero exit (which the runner would mis-map to RunFailed).
func TestRun_UploadURL_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	castDir := t.TempDir()
	args := []string{
		"-cast-dir", castDir,
		"-run", "run-upload-err",
		"-asciinema", "definitely-not-a-real-binary-xyz",
		"-upload-url", srv.URL + "/recording",
		"--", "echo", "x",
	}
	if err := run(args); err != nil {
		t.Fatalf("recording upload failure must be non-fatal, got %v", err)
	}
}
