// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// POST /runs/{id}/exec (run_exec.go): the SSH exec channel over HTTP, no wider.

const execOwner = "owner-sub"

// execHarness is a running run owned by execOwner, on an sshFakeRunner whose ExecStream counts its
// calls and then answers with execFn.
type execHarness struct {
	srv   *Server
	ast   *authzStore
	h     *harness
	id    uuid.UUID
	execs *atomic.Int32
}

func newExecHarness(t *testing.T, execFn func(runner.ExecSpec) (*runner.ExecSession, error)) *execHarness {
	t.Helper()
	execs := &atomic.Int32{}
	srv, ast, h := newResourcesHarness(t, func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		execs.Add(1)
		return execFn(spec)
	})
	return &execHarness{srv: srv, ast: ast, h: h, id: seedResourcesRun(ast, execOwner), execs: execs}
}

// withDataOut makes the run's recorded policy carry rule, or fail to read with readErr.
func (e *execHarness) withDataOut(rule types.TransferRule, readErr error) *execHarness {
	e.srv.cfg.Store = &envelopeStore{Store: e.ast, spec: types.RunPolicySpec{DataOut: rule}, readErr: readErr}
	return e
}

func (e *execHarness) exec(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	owner := ssoSession(t, execOwner, "owner@corp.example", oidc.RoleUser)
	return doSSO(t, e.srv, http.MethodPost, "/api/v1/runs/"+e.id.String()+"/exec", owner, body)
}

// execWithin is exec that fails t when the answer takes longer than bound.
func (e *execHarness) execWithin(t *testing.T, body string, bound time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.exec(t, body) }()
	select {
	case w := <-done:
		return w
	case <-time.After(bound):
		t.Fatalf("POST /exec did not answer within %s", bound)
		return nil
	}
}

func execResult(t *testing.T, w *httptest.ResponseRecorder) client.ExecResult {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("POST /exec = %d %s, want 200", w.Code, w.Body)
	}
	var res client.ExecResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return res
}

func execRows(events []types.AuditEvent, action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range events {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// TestRunExecDataOutRefusedBeforeTheRunner: under review, blocked and an unreadable rule, the command
// never reaches the runner, the answer is 409 data_out_blocked, and the run.data_out.deny row SSH
// writes for a command (path exec) is written, an unreadable rule counting as blocked.
func TestRunExecDataOutRefusedBeforeTheRunner(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rule     types.TransferRule
		readErr  error
		wantRule types.TransferRule
	}{
		{"review", types.TransferReview, nil, types.TransferReview},
		{"blocked", types.TransferBlocked, nil, types.TransferBlocked},
		{"unreadable", "", errors.New("audit store unavailable"), types.TransferBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
				return fakeExecSession("secret", "", 0), nil
			}).withDataOut(tc.rule, tc.readErr)
			w := e.exec(t, `{"command":"cat /etc/hostname"}`)
			if w.Code != http.StatusConflict || wireReason(t, w) != reasonDataOutBlocked {
				t.Errorf("POST /exec = %d %s, want 409 %s", w.Code, w.Body, reasonDataOutBlocked)
			}
			if n := e.execs.Load(); n != 0 {
				t.Errorf("ExecStream was called %d times; the data_out gate runs before the runner", n)
			}
			rows := execRows(e.h.audit.snapshot(), "run.data_out.deny")
			if len(rows) != 1 || rows[0].Outcome != "denied" || !strings.Contains(string(rows[0].Data), `"path":"exec"`) ||
				!strings.Contains(string(rows[0].Data), `"rule":"`+string(tc.wantRule)+`"`) {
				t.Errorf("run.data_out.deny rows = %v, want one denied row with path exec and rule %s", rows, tc.wantRule)
			}
			if rows := execRows(e.h.audit.snapshot(), "run.command.exec"); len(rows) != 0 {
				t.Errorf("a refused command wrote run.command.exec rows: %v", rows)
			}
		})
	}
}

// TestRunExecRunsAsSSHExec: an allowed command runs through sshExecArgv with no TTY and no added
// environment, answers its streams and exit code, and its run.command.exec row holds the command and the exit
// code, never the output.
func TestRunExecRunsAsSSHExec(t *testing.T) {
	var got runner.ExecSpec
	e := newExecHarness(t, func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		got = spec
		return fakeExecSession("out-text", "err-text", 3), nil
	}).withDataOut(types.TransferAllowed, nil)
	res := execResult(t, e.execWithin(t, `{"command":"echo hi"}`, 10*time.Second))
	if res != (client.ExecResult{ExitCode: 3, Stdout: "out-text", Stderr: "err-text"}) {
		t.Errorf("result = %+v", res)
	}
	if !slices.Equal(got.Argv, sshExecArgv("echo hi")) || got.TTY || len(got.Env) != 0 {
		t.Errorf("ExecSpec = %+v, want sshExecArgv(command), no TTY, no env", got)
	}
	rows := execRows(e.h.audit.snapshot(), "run.command.exec")
	if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Actor != execOwner || rows[0].RunID == nil || *rows[0].RunID != e.id {
		t.Fatalf("run.command.exec rows = %v, want one success row for the run by its owner", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil || data["command"] != "echo hi" || data["exit"] != float64(3) || len(data) != 2 {
		t.Errorf("run.command.exec data = %s (%v), want command and exit only", rows[0].Data, err)
	}
}

// floodExecSession writes n bytes to each stream from ONE goroutine, alternating, as the runner's single
// demux does: a reader that stopped on one stream, or read them one after the other, would block it.
func floodExecSession(n int) *runner.ExecSession {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	go func() {
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for sent := 0; sent < n; sent += len(chunk) {
			if _, err := outW.Write(chunk); err != nil {
				break
			}
			if _, err := errW.Write(chunk); err != nil {
				break
			}
		}
		_ = outW.Close()
		_ = errW.Close()
	}()
	return &runner.ExecSession{Stdout: outR, Stderr: errR, Wait: func() (int, error) { return 0, nil }}
}

// TestRunExecTruncatesWithoutStopping: a command writing 10 MiB to each stream ends, keeps the first
// 1 MiB of each and says truncated.
func TestRunExecTruncatesWithoutStopping(t *testing.T) {
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return floodExecSession(10 << 20), nil })
	res := execResult(t, e.execWithin(t, `{"command":"yes"}`, 20*time.Second))
	if !res.Truncated || res.TimedOut || res.ExitCode != 0 || len(res.Stdout) != runExecStreamCap || len(res.Stderr) != runExecStreamCap {
		t.Errorf("result: truncated=%v timed_out=%v exit=%d stdout=%d stderr=%d bytes, want truncated, exit 0 and %d bytes each",
			res.Truncated, res.TimedOut, res.ExitCode, len(res.Stdout), len(res.Stderr), runExecStreamCap)
	}
}

// hangingExecSession is a command that writes nothing and never ends: only Close ends its streams, and
// its Wait returns only after Close.
func hangingExecSession(closed *atomic.Bool) *runner.ExecSession {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	ended := make(chan struct{})
	var once atomic.Bool
	return &runner.ExecSession{
		Stdout: outR,
		Stderr: errR,
		Wait: func() (int, error) {
			<-ended
			return 0, errors.New("exec stream closed")
		},
		Close: func() error {
			if once.CompareAndSwap(false, true) {
				closed.Store(true)
				_ = outW.CloseWithError(io.ErrClosedPipe)
				_ = errW.CloseWithError(io.ErrClosedPipe)
				close(ended)
			}
			return nil
		},
	}
}

// TestRunExecTimeoutEndsTheStream: a 1 s timeout on a command that never ends answers timed_out with
// exit code -1 within a bound, having closed the runner's stream, and its run.command.exec row is a failure.
func TestRunExecTimeoutEndsTheStream(t *testing.T) {
	closed := &atomic.Bool{}
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return hangingExecSession(closed), nil })
	start := time.Now()
	res := execResult(t, e.execWithin(t, `{"command":"sleep 3600","timeout_sec":1}`, 10*time.Second))
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("answered after %s, before the 1 s timeout", elapsed)
	}
	if !res.TimedOut || res.ExitCode != -1 || res.Truncated {
		t.Errorf("result = %+v, want timed_out and exit_code -1", res)
	}
	if !closed.Load() {
		t.Error("the runner's exec stream was not closed on the timeout")
	}
	rows := execRows(e.h.audit.snapshot(), "run.command.exec")
	if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), `"exit":-1`) || !strings.Contains(string(rows[0].Data), "timed out") {
		t.Errorf("run.command.exec rows = %v, want one failure row with exit -1 and the timeout", rows)
	}
}

// TestRunExecRefusals: a bad body, a run that is not running and a runner with no exec stream are
// refused, and none of the first two reaches the runner.
func TestRunExecRefusals(t *testing.T) {
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("", "", 0), nil })
	for body, want := range map[string]string{
		`{"command":"  "}`:                     reasonRunExecInvalid,
		`{"command":"true","timeout_sec":301}`: reasonRunExecInvalid,
		`{"command":"true","timeout_sec":-1}`:  reasonRunExecInvalid,
		`{"command":"true","tty":true}`:        reasonInvalidRequestBody,
	} {
		if w := e.exec(t, body); w.Code != http.StatusBadRequest || wireReason(t, w) != want {
			t.Errorf("%s = %d %s, want 400 %s", body, w.Code, w.Body, want)
		}
	}
	e.ast.mu.Lock()
	run := e.ast.runs[e.id]
	run.State = types.RunCompleted
	e.ast.runs[e.id] = run
	e.ast.mu.Unlock()
	if w := e.exec(t, `{"command":"true"}`); w.Code != http.StatusConflict || wireReason(t, w) != reasonRunExecUnavailable {
		t.Errorf("an ended run = %d %s, want 409 %s", w.Code, w.Body, reasonRunExecUnavailable)
	}
	if n := e.execs.Load(); n != 0 {
		t.Errorf("ExecStream was called %d times for refused requests", n)
	}

	u := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return nil, runner.ErrExecStreamUnsupported })
	if w := u.exec(t, `{"command":"true"}`); w.Code != http.StatusNotImplemented || wireReason(t, w) != reasonRunInspectExecStreamUnsupported {
		t.Errorf("a runner with no exec stream = %d %s, want 501 %s", w.Code, w.Body, reasonRunInspectExecStreamUnsupported)
	}
	if rows := execRows(u.h.audit.snapshot(), "run.command.exec"); len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), `"error":`) {
		t.Errorf("run.command.exec rows = %v, want one failure row carrying the error", rows)
	}
}

// TestRunExecForeignRunIsMissing: a member who does not own the run gets the missing run's 404 and the
// runner is never called.
func TestRunExecForeignRunIsMissing(t *testing.T) {
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("", "", 0), nil })
	mallory := ssoSession(t, "mallory-sub", "mallory@corp.example", oidc.RoleUser)
	if w := doSSO(t, e.srv, http.MethodPost, "/api/v1/runs/"+e.id.String()+"/exec", mallory, `{"command":"id"}`); w.Code != http.StatusNotFound {
		t.Errorf("a non-owner = %d %s, want 404", w.Code, w.Body)
	}
	if n := e.execs.Load(); n != 0 {
		t.Errorf("ExecStream was called %d times for a non-owner", n)
	}
}

// TestRunExecSharesTheSSHChannelCap: a command draws one of the run's SSH channel slots
// (WARDYN_SSH_MAX_SESSIONS_PER_RUN), so a run whose slots are all held answers 429 run_exec_busy without
// reaching the runner, and a finished command gives its slot back.
func TestRunExecSharesTheSSHChannelCap(t *testing.T) {
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("ok", "", 0), nil })
	e.srv.cfg.SSHMaxSessionsPerRun = 1
	if !e.srv.sshAcquireSession(e.id) {
		t.Fatal("could not hold the run's one SSH channel slot")
	}
	w := e.exec(t, `{"command":"true"}`)
	if w.Code != http.StatusTooManyRequests || wireReason(t, w) != reasonRunExecBusy {
		t.Errorf("POST /exec with every slot held = %d %s, want 429 %s", w.Code, w.Body, reasonRunExecBusy)
	}
	if n := e.execs.Load(); n != 0 {
		t.Errorf("ExecStream was called %d times over the cap", n)
	}
	if rows := execRows(e.h.audit.snapshot(), "run.command.exec"); len(rows) != 1 || rows[0].Outcome != "failure" ||
		!strings.Contains(string(rows[0].Data), sshCapReasonChannels) {
		t.Errorf("run.command.exec rows = %v, want one failure naming the channel cap", rows)
	}
	e.srv.sshReleaseSession(e.id)
	execResult(t, e.execWithin(t, `{"command":"true"}`, 10*time.Second))
	if !e.srv.sshAcquireSession(e.id) {
		t.Error("the finished command did not give its slot back")
	}
	e.srv.sshReleaseSession(e.id)
}

// TestRunExecKeepsTheRunAwake: while a command runs, the run's idle clock is reset on the keepalive
// tick, as for an SSH exec, so auto_stop does not end it mid-stream.
func TestRunExecKeepsTheRunAwake(t *testing.T) {
	closed := &atomic.Bool{}
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return hangingExecSession(closed), nil })
	touches := &touchCountingStore{authzStore: e.ast}
	e.srv.cfg.Store = touches
	e.srv.keepaliveEvery = 50 * time.Millisecond
	res := execResult(t, e.execWithin(t, `{"command":"sleep 3600","timeout_sec":1}`, 10*time.Second))
	if !res.TimedOut {
		t.Fatalf("result = %+v, want the timeout to end it", res)
	}
	if n := touches.touches(); n < 3 {
		t.Errorf("the run was touched %d times over a 1 s command with a 50 ms keepalive, want at least 3", n)
	}
}

// brokenExecSession writes out to stdout, then ends both streams with streamErr (nil for a clean end),
// and its Wait answers code and waitErr: a k8s exec whose connection fails after ExecStream returned
// ends its streams and Wait this way (internal/runner/k8s/session.go).
func brokenExecSession(out string, streamErr error, code int, waitErr error) *runner.ExecSession {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	go func() {
		if out != "" {
			_, _ = io.WriteString(outW, out)
		}
		_ = outW.CloseWithError(streamErr)
		_ = errW.CloseWithError(streamErr)
	}()
	return &runner.ExecSession{Stdout: outR, Stderr: errR, Wait: func() (int, error) { return code, waitErr }}
}

// TestRunExecLostCommandIsAFailure: a command the runner never started or lost before it ended answers
// the runner's failure, never a made-up exit code, and its run.command.exec row is a failure carrying
// the error and no exit; a real nonzero exit still answers 200 with its code.
func TestRunExecLostCommandIsAFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sess       func() *runner.ExecSession
		wantStatus int
		wantReason string
	}{
		{"wait fails after an async start failure", func() *runner.ExecSession {
			return brokenExecSession("", nil, 0, errors.New("dial the pod: connection refused"))
		}, http.StatusBadGateway, reasonRunExecFailed},
		{"stream fails at the start", func() *runner.ExecSession {
			return brokenExecSession("", errors.New("upgrade request failed"), 0, nil)
		}, http.StatusBadGateway, reasonRunExecFailed},
		{"partial output, then a read error", func() *runner.ExecSession {
			return brokenExecSession("half of the out", errors.New("connection reset by peer"), 0, nil)
		}, http.StatusBadGateway, reasonRunExecFailed},
		{"the sandbox went away mid-stream", func() *runner.ExecSession {
			return brokenExecSession("half", fmt.Errorf("pod deleted: %w", runner.ErrSandboxGone), 0, nil)
		}, http.StatusConflict, reasonRunExecUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return tc.sess(), nil })
			w := e.execWithin(t, `{"command":"make"}`, 10*time.Second)
			if w.Code != tc.wantStatus || wireReason(t, w) != tc.wantReason {
				t.Errorf("POST /exec = %d %s, want %d %s", w.Code, w.Body, tc.wantStatus, tc.wantReason)
			}
			if strings.Contains(w.Body.String(), "exit_code") || strings.Contains(w.Body.String(), "half") {
				t.Errorf("a lost command answered an exit code or output: %s", w.Body)
			}
			rows := execRows(e.h.audit.snapshot(), "run.command.exec")
			if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), `"error":`) ||
				strings.Contains(string(rows[0].Data), `"exit"`) {
				t.Errorf("run.command.exec rows = %v, want one failure with the error and no exit", rows)
			}
		})
	}

	t.Run("a real exit 1", func(t *testing.T) {
		e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
			return brokenExecSession("FAIL", nil, 1, nil), nil
		})
		res := execResult(t, e.execWithin(t, `{"command":"false"}`, 10*time.Second))
		if res.ExitCode != 1 || res.Stdout != "FAIL" || res.TimedOut {
			t.Errorf("result = %+v, want exit_code 1 with its output", res)
		}
		if rows := execRows(e.h.audit.snapshot(), "run.command.exec"); len(rows) != 1 || rows[0].Outcome != "success" ||
			!strings.Contains(string(rows[0].Data), `"exit":1`) {
			t.Errorf("run.command.exec rows = %v, want one success row with exit 1", rows)
		}
	})
}
