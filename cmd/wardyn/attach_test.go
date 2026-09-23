// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/term"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// writeFile is a small test helper shared across the cmd/wardyn tests for
// materializing temp files (e.g. policy JSON bodies).
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mintOKTicket is the fixed ticket value withMintOK's stub mint endpoint
// hands back — the WS handlers below check the dial actually carries it.
const mintOKTicket = "test-minted-ticket"

// withMintOK wraps a WS handler (the pre-ticket-lane test doubles below all
// had one of these: "accept everything as a WebSocket upgrade") with the mint
// step runAttach now performs FIRST. Without this, the mint's own POST
// request — no Upgrade header, so websocket.Accept always rejects it — would
// be misread as the server's definitive answer and returned to the caller
// before the dial these tests exist to exercise ever ran. Only requests for
// POST .../attach-ticket are intercepted; everything else (the GET dial)
// reaches wsHandler unchanged, so these tests still exercise the real pump.
func withMintOK(wsHandler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attach-ticket") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ticket":"` + mintOKTicket + `"}`))
			return
		}
		wsHandler(w, r)
	}
}

// --------------------------------------------------------------------------
// buildWSURL: HTTP base URL -> WebSocket attach URL
// --------------------------------------------------------------------------

func TestBuildWSURL(t *testing.T) {
	// B12a-F10: attachCmd now refuses a non-UUID run id via parseID before
	// buildWSURL ever sees it (the attach endpoint only ever accepts a UUID),
	// so these fixtures use UUID-shaped ids — the only ones production code
	// still reaches this function with. buildWSURL itself stays a plain string
	// function; it needs no validation of its own.
	tests := []struct {
		name    string
		baseURL string
		runID   string
		ticket  string
		want    string
	}{
		{
			name:    "https becomes wss",
			baseURL: "https://wardyn.example.com",
			runID:   "8f14e45f-ceea-467e-adc5-f4b8e79f5f1c",
			want:    "wss://wardyn.example.com/api/v1/runs/8f14e45f-ceea-467e-adc5-f4b8e79f5f1c/attach",
		},
		{
			name:    "http becomes ws",
			baseURL: "http://localhost:8080",
			runID:   "5b6a5f1e-1c1f-4d3e-8b2a-2f5a6b7c8d9e",
			want:    "ws://localhost:8080/api/v1/runs/5b6a5f1e-1c1f-4d3e-8b2a-2f5a6b7c8d9e/attach",
		},
		{
			name:    "trailing slash on base is trimmed (no doubled slash)",
			baseURL: "https://wardyn.example.com/",
			runID:   "0b3e6a2c-9d4b-4a1f-8e6d-3c2b1a0f9e8d",
			want:    "wss://wardyn.example.com/api/v1/runs/0b3e6a2c-9d4b-4a1f-8e6d-3c2b1a0f9e8d/attach",
		},
		{
			name:    "non-default port preserved",
			baseURL: "http://10.0.0.5:9443",
			runID:   "1a2b3c4d-5e6f-4789-90ab-cdef01234567",
			want:    "ws://10.0.0.5:9443/api/v1/runs/1a2b3c4d-5e6f-4789-90ab-cdef01234567/attach",
		},
		{
			name:    "already-ws scheme left untouched",
			baseURL: "ws://host",
			runID:   "d4c3b2a1-6f5e-4d3c-8b2a-1f0e9d8c7b6a",
			want:    "ws://host/api/v1/runs/d4c3b2a1-6f5e-4d3c-8b2a-1f0e9d8c7b6a/attach",
		},
		{
			name:    "a minted ticket is added as ?ticket= via net/url, not string concat",
			baseURL: "https://wardyn.example.com",
			runID:   "8f14e45f-ceea-467e-adc5-f4b8e79f5f1c",
			ticket:  "abc123def456",
			want:    "wss://wardyn.example.com/api/v1/runs/8f14e45f-ceea-467e-adc5-f4b8e79f5f1c/attach?ticket=abc123def456",
		},
		{
			name:    "a ticket needing escaping is query-encoded, not spliced raw",
			baseURL: "http://localhost:8080",
			runID:   "5b6a5f1e-1c1f-4d3e-8b2a-2f5a6b7c8d9e",
			ticket:  "a b&c=d",
			want:    "ws://localhost:8080/api/v1/runs/5b6a5f1e-1c1f-4d3e-8b2a-2f5a6b7c8d9e/attach?ticket=a+b%26c%3Dd",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildWSURL(tc.baseURL, tc.runID, tc.ticket)
			if got != tc.want {
				t.Errorf("buildWSURL(%q, %q, %q) = %q, want %q", tc.baseURL, tc.runID, tc.ticket, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// isNormalClose: classify clean detaches vs real errors
// --------------------------------------------------------------------------

func TestIsNormalClose(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is a clean close", err: nil, want: true},
		{
			name: "normal closure is clean",
			err:  websocket.CloseError{Code: websocket.StatusNormalClosure},
			want: true,
		},
		{
			name: "going away is clean",
			err:  websocket.CloseError{Code: websocket.StatusGoingAway},
			want: true,
		},
		{
			name: "context canceled is a clean detach",
			err:  context.Canceled,
			want: true,
		},
		{
			name: "wrapped (%w) context canceled is clean via errors.Is",
			err:  fmt.Errorf("read: %w", context.Canceled),
			want: true,
		},
		{
			name: "look-alike string is NOT clean (no substring guessing)",
			err:  errors.New("read: context canceled"),
			want: false,
		},
		{
			name: "abnormal closure is a real error",
			err:  websocket.CloseError{Code: websocket.StatusAbnormalClosure},
			want: false,
		},
		{
			name: "arbitrary error is a real error",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNormalClose(tc.err); got != tc.want {
				t.Errorf("isNormalClose(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// --------------------------------------------------------------------------
// runAttach token guard: an empty token must NOT be a client-side hard
// refusal (H13) — local host-mode deployments run without a token, matching
// the client transport (pkg/client.Client). The dial is attempted and the server's 401 (if auth is
// actually required) is the error signal instead.
// --------------------------------------------------------------------------

func TestRunAttach_NoTokenDialsAnyway(t *testing.T) {
	// No token => runAttach must still attempt the dial rather than refusing
	// client-side. Port 0 on loopback refuses the connection, so we assert the
	// failure is a dial error, not the old hard-refusal message.
	c := &sdk.Client{BaseURL: "http://127.0.0.1:0"}
	err := runAttach(context.Background(), c, "run-1")
	if err == nil {
		t.Fatal("expected a dial error against the bogus address, got nil")
	}
	if got := err.Error(); strings.Contains(got, "no admin token") {
		t.Errorf("error = %q, must not hard-refuse on empty token", got)
	}
}

// TestRunAttach_RejectedHandshakeReturnsAPIError pins that a server-side
// handshake rejection (401/403/404 — never a network failure) comes back as an
// *sdk.APIError carrying the real status + the server's {"error":...} body,
// not as websocket.Dial's raw "failed to WebSocket dial: expected status 101
// but got NNN" text with the HTTP response discarded and exit 1 — so
// exitCodeFor and dialHint classify it exactly like every other API call.
//
// The mint-then-dial lane surfaces THIS test's exact rejection one step
// earlier than its name suggests: this stub answers every request (mint POST
// included) with the same 403, so runAttach now returns it straight from the
// mint attempt without ever reaching websocket.Dial — the same *sdk.APIError
// shape either way. TestRunAttach_ForeignRunMintReturns404 below pins the
// mint-rejection path specifically (a 404, the ticket lane's own shape).
func TestRunAttach_RejectedHandshakeReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"not an operator"}`))
	}))
	defer srv.Close()

	c := &sdk.Client{BaseURL: srv.URL, Token: "tok"}
	err := runAttach(context.Background(), c, "run-1")
	if err == nil {
		t.Fatal("expected an error for a rejected handshake, got nil")
	}
	var ae *sdk.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %v (%T), want an *sdk.APIError (the HTTP rejection must not be discarded)", err, err)
	}
	if ae.Status != http.StatusForbidden {
		t.Errorf("APIError.Status = %d, want %d", ae.Status, http.StatusForbidden)
	}
	if !strings.Contains(err.Error(), "not an operator") {
		t.Errorf("error = %q, want the server's {\"error\":...} message carried through", err.Error())
	}
	if code := exitCodeFor(err); code != 2 {
		t.Errorf("exitCodeFor(403 attach rejection) = %d, want 2 (the universal auth exit code)", code)
	}
}

// --------------------------------------------------------------------------
// B12a-F1: TERM/HUP/INT are wired INSIDE runAttach (never a root
// ExecuteContext) so a signal cancels the session through the SAME path a
// clean pump end already takes: pumpCtx cancels, the pump halves end, and the
// terminal is restored before the command returns. Signals aren't portable to
// send-and-observe in a unit test, so this drives the identical mechanism
// directly — cancelling the ctx runAttach was given reaches the restore call
// exactly as signal.NotifyContext's cancellation would.
// --------------------------------------------------------------------------

func TestRunAttach_CtxCancelRestoresTerminal(t *testing.T) {
	oldMakeRaw, oldRestore := makeRawFn, restoreTerminalFn
	t.Cleanup(func() { makeRawFn, restoreTerminalFn = oldMakeRaw, oldRestore })

	// A real tty is not available under `go test`; inject a fake pair so the
	// restore path is exercised regardless.
	makeRawFn = func(int) (*term.State, error) { return &term.State{}, nil }
	restored := make(chan struct{}, 1)
	restoreTerminalFn = func(int, *term.State) error {
		select {
		case restored <- struct{}{}:
		default:
		}
		return nil
	}

	// os.Stdin under `go test` is not a blocking source — it is typically
	// already at EOF — so half 2 (stdin -> server, attach.go's "Half 2" pump)
	// would get an immediate io.EOF and end the session on ITS OWN, letting
	// this test pass even with the ctx-cancel wiring ripped out entirely (a
	// mutation probe that deletes the wiring stays green on a plain os.Stdin).
	// Swapping in a pipe whose write end this test holds open blocks that half
	// indefinitely, so the cancel below is the only thing that can end the
	// session.
	// Half 2 (attach.go's "Half 2" goroutine, os.Stdin.Read) is a KNOWN,
	// pre-existing leak: os.Stdin.Read is a plain blocking syscall, not
	// ctx-aware, so it stays parked on this pipe even after runAttach
	// returns (bounded only by process exit — see the review's own
	// "Verified OK" note). That leaked goroutine keeps reading the package
	// var os.Stdin, so reassigning `os.Stdin = oldStdin` afterwards races it
	// under -race with no way to synchronize the two (there is no hook into
	// when, or if, that goroutine ever notices the pipe closing). os.Stdin is
	// therefore deliberately left pointing at this (closed) pipe for the
	// rest of the test binary's process — no other test in this package
	// reads the raw global (execCmd/cobra always route stdin through
	// cmd.SetIn, never os.Stdin directly), so nothing downstream depends on
	// restoring it.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = stdinR
	t.Cleanup(func() {
		// Close only the write end: the leaked half-2 read sees EOF and exits, and
		// os.Stdin stays a VALID handle whose reads return EOF (the /dev/null
		// contract), never "file already closed". One fd leaks for the process.
		stdinW.Close()
	})

	srv := httptest.NewServer(withMintOK(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		// Hold the session open; the test drives the end via ctx cancel, not
		// a server-side close.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runAttach(ctx, &sdk.Client{BaseURL: srv.URL}, "run-1")
	}()

	// Let the dial complete and the pump goroutines start, then cancel — the
	// same transition signal.NotifyContext drives on TERM/HUP/INT.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-restored:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal was never restored after ctx cancellation")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runAttach returned %v, want a clean detach (nil)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runAttach did not return after ctx cancellation")
	}
}

// --------------------------------------------------------------------------
// B12a-F1 (review R-02): a signal (or any other caller cancellation) that
// lands WHILE the WebSocket handshake is still in flight must also be a
// clean detach, not a mislabelled "couldn't reach the control plane" — the
// exact mislabelling the whole point of scoping the signal wiring locally
// (rather than a root ExecuteContext) exists to avoid.
// --------------------------------------------------------------------------

func TestRunAttach_CancelledCtxDuringDialIsCleanDetach(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		<-r.Context().Done()
	}))
	defer srv.Close()

	// Already cancelled before the dial ever starts — deterministic, unlike
	// racing a real signal against a live handshake.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runAttach(ctx, &sdk.Client{BaseURL: srv.URL}, "run-1")
	// main.go only calls exitCodeFor when Execute() returns a non-nil error
	// (see main(): the nil case exits 0 implicitly without consulting it) —
	// exitCodeFor(nil) itself falls through every errors.As arm to the
	// catch-all 1, so the real assertion for "clean detach" is err == nil,
	// not a round-trip through exitCodeFor.
	if err != nil {
		t.Fatalf("runAttach with a pre-cancelled ctx = %v, want nil (a clean detach, not a mislabelled dial failure — pre-fix this was a *url.Error wrapping context.Canceled, exitCodeFor 5)", err)
	}
}

// --------------------------------------------------------------------------
// B12a-F1 (review R-01): the pinning test for the SIGNAL WIRING itself — not
// just the cancellation mechanism it feeds — needs a real signal delivered
// to a real process. TestHelperAttachSignal is the child body, re-exec'd
// under an env guard by the two subprocess tests below; it is not a test in
// its own right (it Skips unless the guard env var is set).
// --------------------------------------------------------------------------

func TestHelperAttachSignal(t *testing.T) {
	if os.Getenv("WARDYN_ATTACH_SIGNAL_HELPER") != "1" {
		t.Skip("helper process for the subprocess signal tests; not run directly")
	}
	makeRawFn = func(int) (*term.State, error) { return &term.State{}, nil }
	restoreTerminalFn = func(int, *term.State) error {
		// The parent greps stderr for this marker: proof term.Restore's real
		// (seamed) path actually ran, not just that the process exited.
		fmt.Fprintln(os.Stderr, "restored")
		return nil
	}
	url := os.Getenv("WARDYN_ATTACH_SIGNAL_URL")
	if err := runAttach(context.Background(), &sdk.Client{BaseURL: url}, "run-1"); err != nil {
		t.Fatalf("runAttach: %v", err)
	}
}

// helperCmd builds the re-exec'd child command shared by both subprocess
// signal tests: this same test binary, selecting ONLY TestHelperAttachSignal.
func helperCmd(t *testing.T, url string, stdin, stdout *os.File) *exec.Cmd {
	t.Helper()
	// -test.timeout bounds a re-exec'd binary that `go test` is not supervising;
	// WaitDelay bounds the parent's Wait once the child is signalled or killed.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperAttachSignal$", "-test.timeout=60s")
	cmd.WaitDelay = 10 * time.Second
	t.Cleanup(func() {
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait() // reap; never leave a zombie behind a t.Fatal
		}
	})
	cmd.Env = append(os.Environ(),
		"WARDYN_ATTACH_SIGNAL_HELPER=1",
		"WARDYN_ATTACH_SIGNAL_URL="+url,
	)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	return cmd
}

// A single TERM must end the session cleanly (exit 0) and the terminal must
// actually have been restored (the "restored" stderr marker) — without the
// signal.NotifyContext wiring in runAttach, TERM's default disposition kills
// the child outright: non-zero/signalled exit, no marker, ever.
func TestRunAttach_SIGTERMDetachesCleanly(t *testing.T) {
	accepted := make(chan struct{}, 1)
	srv := httptest.NewServer(withMintOK(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		select {
		case accepted <- struct{}{}:
		default:
		}
		// Read until the child's close frame arrives (sent by conn.Close in
		// runAttach) so the library's own close handshake completes
		// promptly instead of coder/websocket's Close() burning its own 5s
		// peer-ack budget waiting on a server that never reads.
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	// A pipe the parent never writes to: the child's stdin blocks forever
	// (the same shape as a real interactive session), so only the signal can
	// end it.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinW.Close()
	defer stdinR.Close()

	cmd := helperCmd(t, srv.URL, stdinR, nil)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("helper never completed the WS handshake")
	}
	// The server's Accept() returning is not quite the same instant as the
	// CHILD's own websocket.Dial call returning client-side — under heavy
	// system load there is a small window where the child's Dial is still
	// in flight. A TERM landing in exactly that window is a DIFFERENT
	// (also correct) clean-exit path — R-02's dial-cancellation return,
	// which never reaches raw mode / restoreTerminalFn at all — that would
	// make this specific test flaky without pinning what it means to. This
	// margin keeps TERM squarely inside the pump, where restoreTerminalFn is
	// this test's actual target.
	time.Sleep(500 * time.Millisecond)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal helper: %v", err)
	}

	waitErr := cmd.Wait()
	if waitErr != nil {
		t.Fatalf("helper exited with %v (want a clean 0 — SIGTERM must be a clean detach); stderr:\n%s", waitErr, stderr.String())
	}
	if !strings.Contains(stderr.String(), "restored") {
		t.Errorf("helper stderr never printed \"restored\" (term.Restore never ran):\n%s", stderr.String())
	}
}

// A second TERM must still kill the process. The
// signal disposition NotifyContext installs stays redirected until
// stopSignals() runs, deferred all the way to runAttach's own return — so if
// the session is wedged somewhere that does NOT observe ctx (os.Stdout.Write
// is a plain blocking syscall, unlike conn.Read/Write), the FIRST TERM
// cancels ctx but can't unstick the write, and every SUBSEQUENT TERM is
// caught by the same still-registered channel and silently discarded,
// leaving the session unkillable by any number of them. runAttach reverts
// the disposition itself the moment ctx is Done, so this test's second TERM
// falls through to the normal, process-killing default.
//
// The wedge is real, not simulated: the server floods far more than a
// kernel pipe buffer's worth of data (Linux defaults to 64 KiB) at the
// child over the WebSocket, and this test NEVER reads the child's stdout
// pipe — once the buffer fills, the child's os.Stdout.Write blocks and
// stays blocked (conn.Read/Write are ctx-aware; a bare os.File.Write is
// not), regardless of ctx cancellation.
func TestRunAttach_SecondSIGTERMKillsAWedgedSession(t *testing.T) {
	accepted := make(chan struct{}, 1)
	srv := httptest.NewServer(withMintOK(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		select {
		case accepted <- struct{}{}:
		default:
		}
		chunk := bytes.Repeat([]byte("x"), 4096)
		wctx, wcancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer wcancel()
		for range 256 { // 1 MiB total — well past a 64 KiB pipe buffer
			if err := c.Write(wctx, websocket.MessageBinary, chunk); err != nil {
				return
			}
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinW.Close()
	defer stdinR.Close()

	// The child's stdout end of a pipe THIS TEST NEVER READS FROM — the read
	// end must stay OPEN (closing it would make the child's Write fail fast
	// with EPIPE instead of blocking, which would defeat the wedge).
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutR.Close()

	cmd := helperCmd(t, srv.URL, stdinR, stdoutW)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	stdoutW.Close() // this process's copy; the child keeps its own

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("helper never completed the WS handshake")
	}
	// Give the flood time to actually fill the pipe and wedge the write.
	time.Sleep(300 * time.Millisecond)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("first TERM: %v", err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case err := <-waitDone:
		t.Fatalf("helper exited after ONE TERM (%v) — the wedge scenario did not reproduce; stderr:\n%s", err, stderr.String())
	case <-time.After(500 * time.Millisecond):
		// Still alive: ctx was cancelled, but the wedged stdout Write can't
		// observe that. Expected.
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("second TERM: %v", err)
	}

	select {
	case <-waitDone:
		// Gone — the second TERM's reverted (default) disposition killed it.
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("a second TERM never killed the wedged session — signal disposition was not reverted after the first")
	}
}

// --------------------------------------------------------------------------
// B12a-F10: attach validates the run id client-side, exactly like `ssh`
// already does (ssh_test.go's TestRunSSH_RefusesANonUUIDRunID) — the id is
// spliced straight into the WebSocket dial URL (buildWSURL) with no further
// encoding, and the attach endpoint only ever accepts a UUID.
// --------------------------------------------------------------------------

func TestAttachCmd_RefusesANonUUIDRunID(t *testing.T) {
	hostile := []string{
		"run_x/../../evil",
		"run_x?foo=bar",
		"run_x#frag",
		"",
	}
	for _, id := range hostile {
		t.Run(id, func(t *testing.T) {
			err := execCmd(t, "attach", "--", id)
			if err == nil {
				t.Fatalf("%q was accepted", id)
			}
			if !strings.Contains(err.Error(), "invalid run id") {
				t.Errorf("error = %q, want it to name an invalid run id", err.Error())
			}
		})
	}
}

// --------------------------------------------------------------------------
// `wardyn attach` mints a single-use attach ticket with whatever token is
// configured (POST /runs/{id}/attach-ticket, owner-or-admin) and dials
// with it, instead of dialing the WS route directly with a bearer that
// route's fallback lane requires be an admin's. These four pin that
// contract: a member's own
// token mints and dials; a foreign run gets the ticket lane's 404 (no
// existence oracle); an admin token still works; the ticket is freshly
// minted on every attach attempt, never cached or reused.
// --------------------------------------------------------------------------

// TestRunAttach_MintsTicketThenDialsWithIt is the member success path: a
// non-admin caller's own configured token is enough to mint (the mint
// endpoint is owner-or-admin, not operator-only) and the CLI then dials with
// the ticket the mint returned — not the bare bearer, which the WS route's
// fallback lane would refuse for a member. This is the test that fails if the
// mint step is ever removed: without it there is no ?ticket= to check for,
// and the dial would simply not happen against this stub (it 403s any
// ticket-less GET).
func TestRunAttach_MintsTicketThenDialsWithIt(t *testing.T) {
	const memberToken = "member-own-token"
	const mintedTicket = "minted-for-member-run"
	mintSawToken := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attach-ticket") {
			if r.Header.Get("Authorization") == "Bearer "+memberToken {
				mintSawToken = true
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ticket":"` + mintedTicket + `"}`))
			return
		}
		// The dial: only succeeds when it carries the ticket the mint just
		// handed back — proves the CLI dialed WITH the minted ticket, not the
		// bare bearer the pre-ticket-lane CLI would have sent instead.
		if r.URL.Query().Get("ticket") != mintedTicket {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runAttach(ctx, &sdk.Client{BaseURL: srv.URL, Token: memberToken}, "run-1")
	if err != nil {
		t.Fatalf("runAttach = %v, want nil (mint then ticket dial should succeed for a member's own token)", err)
	}
	if !mintSawToken {
		t.Error("the mint request never carried the configured token as a bearer")
	}
}

// TestRunAttach_AdminTokenMintsAndDials: an admin-token caller (e.g. CI) is
// unaffected by the ticket lane — the mint endpoint authorizes an admin on
// ANY run (attach_ticket.go's isOperator arm), so the same mint-then-dial
// flow that serves a member also serves an admin token, with no change in
// outcome.
func TestRunAttach_AdminTokenMintsAndDials(t *testing.T) {
	const adminTicket = "minted-for-admin"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attach-ticket") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ticket":"` + adminTicket + `"}`))
			return
		}
		if r.URL.Query().Get("ticket") != adminTicket {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runAttach(ctx, &sdk.Client{BaseURL: srv.URL, Token: "admin-bearer-token"}, "run-1"); err != nil {
		t.Fatalf("runAttach with an admin token = %v, want nil", err)
	}
}

// TestRunAttach_ForeignRunMintReturns404: the goal's audit-shape change. A
// run the caller does not own now refuses at MINT time with the byte-
// identical 404 a nonexistent run gets (getRunAuthorized's no-existence-
// oracle rule, internal/api/helpers.go) — never the WS route's old blanket
// 403 (requireOperator), which never even loaded the run to check. The dial
// must never be attempted once the mint has definitively refused.
func TestRunAttach_ForeignRunMintReturns404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attach-ticket") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"run not found"}`))
			return
		}
		t.Error("a dial was attempted after a 404 mint refusal — a foreign run must not fall through to a bare dial")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := runAttach(context.Background(), &sdk.Client{BaseURL: srv.URL, Token: "member-token"}, "run-1")
	var ae *sdk.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v (%T), want an *sdk.APIError", err, err)
	}
	if ae.Status != http.StatusNotFound {
		t.Errorf("APIError.Status = %d, want 404 (no existence oracle)", ae.Status)
	}
}

// TestRunAttach_TicketIsReMintedEachAttach: the ticket is single-use with a
// 30s TTL (consumed by a DELETE-and-return on first redemption,
// internal/api/attach_ticket.go's consumeAttachTicket) — a second attach must
// mint its OWN fresh ticket, never replay a ticket a previous attempt already
// spent. The stub hands out a distinct ticket per mint call and only accepts
// a dial carrying the MOST RECENTLY minted one, so a cached/reused ticket
// from attempt 1 would be refused on attempt 2.
func TestRunAttach_TicketIsReMintedEachAttach(t *testing.T) {
	var mintCount int32
	var lastTicket atomic.Value
	lastTicket.Store("")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attach-ticket") {
			n := atomic.AddInt32(&mintCount, 1)
			tok := fmt.Sprintf("ticket-%d", n)
			lastTicket.Store(tok)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ticket":"` + tok + `"}`))
			return
		}
		if r.URL.Query().Get("ticket") != lastTicket.Load().(string) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		<-r.Context().Done()
	}))
	defer srv.Close()

	for i := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := runAttach(ctx, &sdk.Client{BaseURL: srv.URL, Token: "member-token"}, "run-1")
		cancel()
		if err != nil {
			t.Fatalf("attempt %d: runAttach = %v, want nil", i, err)
		}
	}
	if got := atomic.LoadInt32(&mintCount); got != 2 {
		t.Errorf("mint count = %d, want 2 — the ticket must be re-minted per attempt, never cached or reused", got)
	}
}

// TestRunAttach_FallsBackToBareDialWhenMintUnavailable pins the goal's other
// requirement: "keep the admin/bearer path working exactly as now when no
// ticket can be minted, so an admin-token CI caller is unaffected." No
// /attach-ticket route is registered here (an older control plane); the mux's
// own 404 page is not the JSON `{"ticket":...}` shape mintAttachTicket knows
// how to read, so it is treated as inconclusive rather than a definitive
// refusal, and the CLI falls back to dialing directly with the configured
// bearer — exactly this command's behavior before the ticket lane existed.
func TestRunAttach_FallsBackToBareDialWhenMintUnavailable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/runs/run-1/attach", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ticket") != "" {
			t.Error("the fallback dial must not carry a ticket query param")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer admin-bearer-token" {
			t.Errorf("Authorization = %q, want the configured bearer (the pre-ticket-lane dial)", got)
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runAttach(ctx, &sdk.Client{BaseURL: srv.URL, Token: "admin-bearer-token"}, "run-1"); err != nil {
		t.Fatalf("runAttach = %v, want nil (the legacy bearer dial must still work)", err)
	}
}

// --------------------------------------------------------------------------
// The attach-mode frame (internal/api/attach_holder.go's attachModeMsg) is
// a text frame Half 1 must surface, not skip — otherwise someone attached
// read-only types, nothing happens, and nothing explains why. These four
// pin that and the one contract it must never break: stdout stays
// byte-identical PTY output, nothing else, in every case below.
// --------------------------------------------------------------------------

// sharedAttachStdinOnce / sharedAttachStdinW back swapSharedAttachStdin below.
var (
	sharedAttachStdinOnce sync.Once
	sharedAttachStdinW    *os.File
)

// swapSharedAttachStdin points os.Stdin at a pipe this test binary never
// closes, exactly ONCE for every test in this file that needs one — mirroring
// the os.Stdin swap TestRunAttach_CtxCancelRestoresTerminal already relies on
// (see its own comment on why: go test's real os.Stdin is not a blocking
// source, so Half 2 would race a premature EOF-driven cancel in ahead of the
// frames these tests need Half 1 to process first).
//
// It is package-shared and NEVER reassigned again (sync.Once), on purpose:
// os.Stdin.Read (attach.go's Half 2) is not ctx-aware, so a pump reading a
// PRIOR test's pipe can still be mid-syscall, unsynchronized, when a LATER
// test's setup runs — reassigning the os.Stdin global at that moment is a
// write racing that read with no happens-before edge between them, which
// -race reports even though the two are logically unrelated (one earlier
// test's pipe never gets closed here either, matching that same test's own
// documented "leaked forever" acceptance). Handing out the same never-closed
// pipe to every caller means the os.Stdin global is written at most once for
// the whole file, so there is nothing left for a later test to race against.
func swapSharedAttachStdin(t *testing.T) *os.File {
	t.Helper()
	sharedAttachStdinOnce.Do(func() {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdin = r
		sharedAttachStdinW = w
	})
	return sharedAttachStdinW
}

// redirectAttachIO swaps os.Stdin (once, package-wide — see
// swapSharedAttachStdin) and os.Stdout/os.Stderr (per call) for pipes the
// test controls.
func redirectAttachIO(t *testing.T) (stdinW *os.File, stdout, stderr func() string) {
	t.Helper()
	stdinW = swapSharedAttachStdin(t)

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = stdoutW

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = stderrW

	// stdinW is the PACKAGE-SHARED write end (swapSharedAttachStdin) and is
	// deliberately never closed here — closing it would EOF every other
	// test's Half 2 too, including ones that haven't run yet.

	// drain, called AFTER runAttach returns: closes the write ends (unblocking
	// the readers), restores the real os.Stdout/os.Stderr, and returns each
	// stream's captured content.
	drain := func(w *os.File, r *os.File, old *os.File, target **os.File) func() string {
		return func() string {
			w.Close()
			*target = old
			var buf bytes.Buffer
			_, _ = io.Copy(&buf, r)
			return buf.String()
		}
	}
	return stdinW,
		drain(stdoutW, stdoutR, oldStdout, &os.Stdout),
		drain(stderrW, stderrR, oldStderr, &os.Stderr)
}

// TestAttach_ReadOnlyDialPrintsNoticeAndDetachesCleanly pins the read-only
// half of the fix: the first attach-mode frame (read_only:true) prints ONE
// stderr line naming the holder and where they attached from, stdout carries
// only the real PTY bytes, and the session still exits 0 (a clean detach) on
// the server's own normal close.
func TestAttach_ReadOnlyDialPrintsNoticeAndDetachesCleanly(t *testing.T) {
	srv := httptest.NewServer(withMintOK(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		mode := `{"type":"attach-mode","read_only":true,"holder":{"held":true,"principal":"alice@example.com","source":"web"}}`
		if err := c.Write(r.Context(), websocket.MessageText, []byte(mode)); err != nil {
			return
		}
		if err := c.Write(r.Context(), websocket.MessageBinary, []byte("server-output")); err != nil {
			return
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	_, stdout, stderr := redirectAttachIO(t)

	attachErr := runAttach(context.Background(), &sdk.Client{BaseURL: srv.URL}, "run-1")
	stdoutGot, stderrGot := stdout(), stderr()

	if attachErr != nil {
		t.Fatalf("runAttach = %v, want nil (still a clean detach when read-only)", attachErr)
	}
	if stdoutGot != "server-output" {
		t.Errorf("stdout = %q, want exactly the PTY bytes and nothing else", stdoutGot)
	}
	if !strings.Contains(stderrGot, "alice@example.com") || !strings.Contains(stderrGot, "browser") {
		t.Errorf("stderr = %q, want the read-only notice naming the holder and the source", stderrGot)
	}
	if !strings.Contains(stderrGot, "detached") {
		t.Errorf("stderr = %q, want the usual final \"detached\" line too", stderrGot)
	}
}

// TestAttach_PromotionPrintsOneLineAndResendsResizeOnce pins the
// promotion half: a SECOND attach-mode frame whose read_only flips
// true->false prints exactly one promotion line and re-sends the window size
// exactly once (never on the frame that made this socket the writer in the
// first place, and never more than once for one promotion).
func TestAttach_PromotionPrintsOneLineAndResendsResizeOnce(t *testing.T) {
	oldMakeRaw, oldRestore, oldGetSize := makeRawFn, restoreTerminalFn, getSizeFn
	t.Cleanup(func() { makeRawFn, restoreTerminalFn, getSizeFn = oldMakeRaw, oldRestore, oldGetSize })
	// A real tty is not available under `go test`; fake the raw-mode and
	// GetSize seams so the promotion's resend-the-window-size path (gated on
	// oldState != nil) actually runs.
	makeRawFn = func(int) (*term.State, error) { return &term.State{}, nil }
	restoreTerminalFn = func(int, *term.State) error { return nil }
	getSizeFn = func(int) (int, int, error) { return 80, 24, nil }

	var resizeCount int32
	resizeSeen := make(chan int32, 8)

	srv := httptest.NewServer(withMintOK(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()

		go func() {
			for {
				typ, data, rerr := c.Read(r.Context())
				if rerr != nil {
					return
				}
				if typ != websocket.MessageText {
					continue
				}
				var m map[string]any
				if json.Unmarshal(data, &m) == nil && m["type"] == "resize" {
					resizeSeen <- atomic.AddInt32(&resizeCount, 1)
				}
			}
		}()

		mode := `{"type":"attach-mode","read_only":true,"holder":{"held":true,"principal":"alice@example.com","source":"web"}}`
		if err := c.Write(r.Context(), websocket.MessageText, []byte(mode)); err != nil {
			return
		}

		// The client's own initial connect-time resize (attach.go's, unrelated
		// to promotion) never fires here: it sizes from the REAL term.GetSize
		// against the fd behind os.Stdin, which is a pipe under this test, not
		// a tty — so the only resize frame this exchange can ever produce is
		// the promotion's own, sized from the faked getSizeFn seam above.
		promo := `{"type":"attach-mode","read_only":false,"holder":{"held":true,"principal":"me@example.com","source":"web"}}`
		if err := c.Write(r.Context(), websocket.MessageText, []byte(promo)); err != nil {
			return
		}

		select {
		case n := <-resizeSeen:
			if n != 1 {
				t.Errorf("resize frame count after promotion = %d, want exactly 1", n)
			}
		case <-time.After(2 * time.Second):
			t.Error("promotion never re-sent the resize frame")
		}

		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	_, stdout, stderr := redirectAttachIO(t)

	attachErr := runAttach(context.Background(), &sdk.Client{BaseURL: srv.URL}, "run-1")
	stdoutGot, stderrGot := stdout(), stderr()

	if attachErr != nil {
		t.Fatalf("runAttach = %v, want nil", attachErr)
	}
	if stdoutGot != "" {
		t.Errorf("stdout = %q, want no PTY output leaked by the promotion exchange", stdoutGot)
	}
	if n := strings.Count(stderrGot, "promoted"); n != 1 {
		t.Errorf("stderr contains %d promotion line(s) (%q), want exactly 1", n, stderrGot)
	}
	if got := atomic.LoadInt32(&resizeCount); got != 1 {
		t.Errorf("total resize frames received by the server = %d, want exactly 1", got)
	}
}

// TestAttach_UnknownAttachModeFrameIsIgnored pins that an attach-mode-
// shaped frame this CLI does not recognize (a future control frame type)
// is silently dropped — never crashing the process, never printing noise,
// and never interrupting the pump for the binary frames around it.
func TestAttach_UnknownAttachModeFrameIsIgnored(t *testing.T) {
	srv := httptest.NewServer(withMintOK(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		// The writer case (read_only:false) is silent — nothing to warn about.
		mode := `{"type":"attach-mode","read_only":false,"holder":{"held":true,"principal":"me@example.com","source":"web"}}`
		if err := c.Write(r.Context(), websocket.MessageText, []byte(mode)); err != nil {
			return
		}
		unknown := `{"type":"some-future-control-frame","foo":"bar"}`
		if err := c.Write(r.Context(), websocket.MessageText, []byte(unknown)); err != nil {
			return
		}
		if err := c.Write(r.Context(), websocket.MessageBinary, []byte("after-unknown")); err != nil {
			return
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	_, stdout, stderr := redirectAttachIO(t)

	attachErr := runAttach(context.Background(), &sdk.Client{BaseURL: srv.URL}, "run-1")
	stdoutGot, stderrGot := stdout(), stderr()

	if attachErr != nil {
		t.Fatalf("runAttach = %v, want nil", attachErr)
	}
	if stdoutGot != "after-unknown" {
		t.Errorf("stdout = %q, want exactly the PTY bytes around the unknown frame, nothing dropped or corrupted", stdoutGot)
	}
	if want := "detached\n"; stderrGot != want {
		t.Errorf("stderr = %q, want only %q — no noise for the writer connect or the unrecognised frame", stderrGot, want)
	}
}

// TestAttach_TakeoverCloseIsNotACleanDetach pins the corpus item this
// package's own contract most needs pinned: a take-over's 1008
// (StatusPolicyViolation) close — attach.go's displace() closure on the
// server — must never be reported the same as a normal detach.
func TestAttach_TakeoverCloseIsNotACleanDetach(t *testing.T) {
	srv := httptest.NewServer(withMintOK(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		mode := `{"type":"attach-mode","read_only":false,"holder":{"held":true,"principal":"me@example.com","source":"web"}}`
		if err := c.Write(r.Context(), websocket.MessageText, []byte(mode)); err != nil {
			return
		}
		_ = c.Close(websocket.StatusPolicyViolation, "taken over by bob@example.com")
	}))
	defer srv.Close()

	_, stdout, stderr := redirectAttachIO(t)

	attachErr := runAttach(context.Background(), &sdk.Client{BaseURL: srv.URL}, "run-1")
	stdoutGot, stderrGot := stdout(), stderr()

	if attachErr == nil {
		t.Fatal("runAttach = nil, want an error — a take-over close must not be reported as a clean detach")
	}
	if cs := websocket.CloseStatus(attachErr); cs != websocket.StatusPolicyViolation {
		t.Errorf("CloseStatus(err) = %v, want StatusPolicyViolation (1008)", cs)
	}
	if strings.Contains(stderrGot, "detached") {
		t.Errorf("stderr = %q, must not print the clean-detach line on a take-over", stderrGot)
	}
	if stdoutGot != "" {
		t.Errorf("stdout = %q, want no PTY output leaked", stdoutGot)
	}
}
