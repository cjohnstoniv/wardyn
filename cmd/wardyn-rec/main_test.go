// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

// TestBuildAsciinemaArgv_PreventsShellInjection pins that the agent argv
// cannot inject shell commands. The agent argv (untrusted run-task/command
// data) is
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

// The -c trailer must capture the AGENT's real exit code to the sidecar file so
// runAsciinema can recover it (asciinema's own exit code masks a failed agent —
// the fail-open hazard the BYOI selftest gate depends on avoiding).
func TestBuildAsciinemaArgv_CapturesAgentExitCode(t *testing.T) {
	cast := filepath.Join(t.TempDir(), "out.cast")
	// A recorded "agent" that exits 3 (stands in for a failing agent-run --selftest).
	argv := buildAsciinemaArgv("/usr/bin/asciinema", cast, []string{"sh", "-c", "exit 3"})
	var cString string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-c" {
			cString = argv[i+1]
			break
		}
	}
	// Drive the -c string through a shell as asciinema would (asciinema itself
	// would exit 0 here; we only care that the sidecar captured the agent's 3).
	_ = exec.Command("sh", "-c", cString).Run()
	code, ok := readAgentExitFile(cast)
	if !ok {
		t.Fatal("sidecar exit file was not written by the -c trailer")
	}
	if code != 3 {
		t.Fatalf("recovered exit code = %d, want 3 (asciinema's masked 0 must not win)", code)
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

// TestRun_UploadURL_ServerError pins that a failed recording upload is
// non-fatal. The agent already ran, so wardyn-rec must not turn a delivery
// error into a non-zero exit (which the runner would mis-map to RunFailed).
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

// TestUploadCast_BoundedTimeout pins the recorder's upload bound: a server
// that accepts the connection but never responds (the shape of a control plane
// that is unreachable behind a mesh sidecar that still completes the TCP
// handshake) must not be able to hold uploadCast open for anything like a bare
// 60s. Shrinks the package's upload timeouts so the test runs fast rather than
// taking the full real bound.
func TestUploadCast_BoundedTimeout(t *testing.T) {
	origDial, origClient := uploadDialTimeout, uploadClientTimeout
	uploadDialTimeout = 200 * time.Millisecond
	uploadClientTimeout = 500 * time.Millisecond
	t.Cleanup(func() { uploadDialTimeout, uploadClientTimeout = origDial, origClient })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			// Accept and hold: never read or write, so the client hangs
			// waiting for a response that never comes.
			_ = conn
		}
	}()

	cast := filepath.Join(t.TempDir(), "out.cast")
	if err := os.WriteFile(cast, []byte("test cast data"), 0o600); err != nil {
		t.Fatalf("write cast: %v", err)
	}

	start := time.Now()
	uerr := uploadCast(cast, "http://"+ln.Addr().String()+"/recording", "")
	elapsed := time.Since(start)
	if uerr == nil {
		t.Fatal("uploadCast: want an error (the server never responds), got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("uploadCast took %s, want it bounded well under 2s (shrunk timeouts: dial=%s client=%s)",
			elapsed, uploadDialTimeout, uploadClientTimeout)
	}
}

// stubAsciinema is a shell stand-in for asciinema 2.4.0: it refuses an
// existing non-empty cast (exit 1, command not run) and otherwise runs the -c
// command and records a cast. STUB_MODE=kill leaves a cast and exits 3 without
// running the command, as a killed recorder would.
const stubAsciinema = `#!/bin/sh
# rec --stdin -q -c <cmd> <cast>
cmd=$5; cast=$6
if [ -s "$cast" ]; then echo "asciinema: $cast exists" >&2; exit 1; fi
if [ "$STUB_MODE" = kill ]; then echo hdr > "$cast"; exit 3; fi
sh -c "$cmd"
echo hdr > "$cast"
exit 0
`

// TestRecHelper is the child half of recOnce: it runs wardyn-rec's run() in a
// fresh process, because run exits with the task's non-zero code.
func TestRecHelper(t *testing.T) {
	raw := os.Getenv("WARDYN_REC_TEST_ARGS")
	if raw == "" {
		t.Skip("child process only")
	}
	if err := run(strings.Split(raw, "\x1f")); err != nil {
		os.Stderr.WriteString("wardyn-rec: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(0)
}

func recOnce(t *testing.T, castDir, stub, mode, cmd string) (int, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-cast-dir", castDir, "-run", "run-1", "-asciinema", stub,
		"-out-dir", t.TempDir(), "--", "sh", "-c", cmd}
	c := exec.Command(exe, "-test.run=^TestRecHelper$") //nolint:gosec
	c.Env = append(os.Environ(), "WARDYN_REC_TEST_ARGS="+strings.Join(args, "\x1f"), "STUB_MODE="+mode)
	out, err := c.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatal(err)
	}
	return ee.ExitCode(), string(out)
}

func writeStub(t *testing.T) string {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "asciinema")
	if err := os.WriteFile(stub, []byte(stubAsciinema), 0o700); err != nil {
		t.Fatal(err)
	}
	return stub
}

// TestRun_SecondCallSameRunRunsTask: the selftest leaves a cast and an exit
// file holding 0; the task's call under the same run id must still run and
// return the task's own code, zero or not.
func TestRun_SecondCallSameRunRunsTask(t *testing.T) {
	for _, want := range []int{0, 5} {
		t.Run(strconv.Itoa(want), func(t *testing.T) {
			dir, stub := t.TempDir(), writeStub(t)
			if code, out := recOnce(t, dir, stub, "", "exit 0"); code != 0 {
				t.Fatalf("selftest call = %d, want 0\n%s", code, out)
			}
			marker := filepath.Join(t.TempDir(), "task-ran")
			code, out := recOnce(t, dir, stub, "", "touch "+marker+"; exit "+strconv.Itoa(want))
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("task did not run after the selftest's recording: %v\n%s", err, out)
			}
			if code != want {
				t.Fatalf("task call = %d, want %d\n%s", code, want, out)
			}
		})
	}
}

// TestRun_NoFreshExitFileReturnsAsciinemaCode: a recorder that dies before the
// trailer writes the exit file must not be read back as the stale 0.
func TestRun_NoFreshExitFileReturnsAsciinemaCode(t *testing.T) {
	dir, stub := t.TempDir(), writeStub(t)
	if code, out := recOnce(t, dir, stub, "", "exit 0"); code != 0 {
		t.Fatalf("selftest call = %d, want 0\n%s", code, out)
	}
	if code, out := recOnce(t, dir, stub, "kill", "exit 0"); code != 3 {
		t.Fatalf("killed call = %d, want asciinema's 3\n%s", code, out)
	}
}

// TestRun_StaleRemovalFailureRefuses: anything but "absent" stops the call
// before asciinema runs.
func TestRun_StaleRemovalFailureRefuses(t *testing.T) {
	dir, stub := t.TempDir(), writeStub(t)
	exit := filepath.Join(dir, "run-1.exit")
	if err := os.MkdirAll(filepath.Join(exit, "keep"), 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "task-ran")
	code, out := recOnce(t, dir, stub, "", "touch "+marker)
	if code == 0 || !strings.Contains(out, "remove stale recording file") {
		t.Fatalf("code = %d, want a removal refusal\n%s", code, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("task ran despite the removal failure")
	}
}

// A SIGTERM aimed at wardyn-rec (here sent by the child to its parent) reaches the child, so the run ends and its
// cast can be delivered instead of wardyn-rec dying with the child still recording.
func TestRunAsciinemaForwardsTermination(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "asciinema")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nkill -TERM $PPID\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The test process absorbs the signal itself, so a missing forward fails the wait below rather than
	// killing the test binary.
	absorb := make(chan os.Signal, 1)
	signal.Notify(absorb, syscall.SIGTERM)
	defer signal.Stop(absorb)
	done := make(chan error, 1)
	go func() {
		_, err := runAsciinema(fake, filepath.Join(dir, "r.cast"), []string{"true"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runAsciinema: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the child was still recording 10 s after SIGTERM: it was not forwarded")
	}
}
