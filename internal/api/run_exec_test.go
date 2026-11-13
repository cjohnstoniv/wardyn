// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
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
		// Past the most one exec argument may be: refused before anything is audited or run.
		`{"command":"` + strings.Repeat("x", 200<<10) + `"}`: reasonRunExecInvalid,
	} {
		if w := e.exec(t, body); w.Code != http.StatusBadRequest || wireReason(t, w) != want {
			t.Errorf("%.60s = %d %.200s, want 400 %s", body, w.Code, w.Body, want)
		}
	}
	if rows := execRows(e.h.audit.snapshot(), "run.command.exec"); len(rows) != 0 {
		t.Errorf("refused bodies wrote run.command.exec rows: %d", len(rows))
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

// background makes the harness's run a background task (experience: background).
func (e *execHarness) background() {
	e.ast.mu.Lock()
	defer e.ast.mu.Unlock()
	run := e.ast.runs[e.id]
	run.Experience = types.ExperienceBackground
	e.ast.runs[e.id] = run
}

// underProfile launches the harness's run under a governance profile named "ci" that carries limits.
func (e *execHarness) underProfile(limits types.GovernanceLimits) {
	profile := types.GovernanceUnit{ID: uuid.New(), Name: "ci", Limits: limits}
	e.ast.mu.Lock()
	run := e.ast.runs[e.id]
	run.UnitID = &profile.ID
	e.ast.runs[e.id] = run
	e.ast.mu.Unlock()
	e.srv.cfg.Store = &profileListStore{authzStore: e.ast, profiles: []types.GovernanceUnit{profile}}
}

// execError is the error sentence of a refused POST /exec.
func execError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return body.Error
}

// TestRunExecBackgroundRunTakesACommand: a background task takes a command through this route, though it has
// no SSH access (TestSSHFreshRun_BackgroundRunRefusesEveryChannel): the runner is called once and the
// run.command.exec row holds the command and its exit code.
func TestRunExecBackgroundRunTakesACommand(t *testing.T) {
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("uid=1000", "", 0), nil })
	e.background()
	res := execResult(t, e.execWithin(t, `{"command":"id"}`, 10*time.Second))
	if res.ExitCode != 0 || res.Stdout != "uid=1000" {
		t.Errorf("result = %+v, want exit 0 and the command's output", res)
	}
	if n := e.execs.Load(); n != 1 {
		t.Errorf("ExecStream was called %d times, want 1", n)
	}
	if rows := execRows(e.h.audit.snapshot(), "run.command.exec"); len(rows) != 1 || rows[0].Outcome != "success" ||
		!strings.Contains(string(rows[0].Data), `"command":"id"`) {
		t.Errorf("run.command.exec rows = %v, want one success row naming the command", rows)
	}
}

// TestRunExecBackgroundRunKeepsTheDoors: a background run's command meets every door an interactive run's does,
// before the runner is called, and is answered in words a pipeline step can read: the governance profile
// (deny_interactive on any run, deny_task_mode_exec on a background run alone), the data_out rule, the channel
// cap and the run's state. A sandbox with nothing to exec into answers 501 in this route's own words.
func TestRunExecBackgroundRunKeepsTheDoors(t *testing.T) {
	ok := func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("", "", 0), nil }
	refusedFirst := func(t *testing.T, e *execHarness, w *httptest.ResponseRecorder, code int, reason string) {
		t.Helper()
		if w.Code != code || wireReason(t, w) != reason {
			t.Errorf("POST /exec = %d %s, want %d %s", w.Code, w.Body, code, reason)
		}
		if n := e.execs.Load(); n != 0 {
			t.Errorf("ExecStream was called %d times; the door is before the runner", n)
		}
	}
	for _, tc := range []struct {
		name   string
		limits types.GovernanceLimits
		msg    string
	}{
		{"deny_interactive", types.GovernanceLimits{DenyInteractive: true}, runExecDenyInteractiveMsg},
		{"deny_task_mode_exec", types.GovernanceLimits{DenyTaskModeExec: true}, runExecDenyTaskModeExecMsg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExecHarness(t, ok)
			e.background()
			e.underProfile(tc.limits)
			w := e.exec(t, `{"command":"id"}`)
			refusedFirst(t, e, w, http.StatusForbidden, string(authz.ReasonGovernanceUnit))
			if got, want := execError(t, w), fmt.Sprintf(tc.msg, "ci"); got != want {
				t.Errorf("error = %q, want %q", got, want)
			}
			if !recHasRefusal(e.h.audit, authz.ReasonGovernanceUnit, "runs.exec") {
				t.Error("no authz.denied governance_unit row for runs.exec")
			}
		})
	}
	t.Run("deny_task_mode_exec leaves an interactive run's command alone", func(t *testing.T) {
		e := newExecHarness(t, ok)
		e.underProfile(types.GovernanceLimits{DenyTaskModeExec: true})
		execResult(t, e.exec(t, `{"command":"id"}`))
		if n := e.execs.Load(); n != 1 {
			t.Errorf("ExecStream was called %d times, want 1", n)
		}
	})
	t.Run("data_out", func(t *testing.T) {
		e := newExecHarness(t, ok)
		e.background()
		e.withDataOut(types.TransferBlocked, nil)
		refusedFirst(t, e, e.exec(t, `{"command":"id"}`), http.StatusConflict, reasonDataOutBlocked)
	})
	t.Run("channel cap", func(t *testing.T) {
		e := newExecHarness(t, ok)
		e.background()
		e.srv.cfg.SSHMaxSessionsPerRun = 1
		if !e.srv.sshAcquireSession(e.id) {
			t.Fatal("could not hold the run's one channel slot")
		}
		defer e.srv.sshReleaseSession(e.id)
		refusedFirst(t, e, e.exec(t, `{"command":"id"}`), http.StatusTooManyRequests, reasonRunExecBusy)
	})
	t.Run("ended", func(t *testing.T) {
		e := newExecHarness(t, ok)
		e.background()
		e.ast.mu.Lock()
		run := e.ast.runs[e.id]
		run.State = types.RunCompleted
		e.ast.runs[e.id] = run
		e.ast.mu.Unlock()
		w := e.exec(t, `{"command":"id"}`)
		refusedFirst(t, e, w, http.StatusConflict, reasonRunExecUnavailable)
		if got, want := execError(t, w), fmt.Sprintf(execLiveRunWords.notRunning, types.RunCompleted); got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
	})
	t.Run("a sandbox with nothing to exec into", func(t *testing.T) {
		e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return nil, runner.ErrExecStreamUnsupported })
		e.background()
		w := e.exec(t, `{"command":"id"}`)
		if w.Code != http.StatusNotImplemented || wireReason(t, w) != reasonRunInspectExecStreamUnsupported || execError(t, w) != runExecUnsupportedMsg {
			t.Errorf("POST /exec = %d %s, want 501 %s saying %q", w.Code, w.Body, reasonRunInspectExecStreamUnsupported, runExecUnsupportedMsg)
		}
		if rows := execRows(e.h.audit.snapshot(), "run.command.exec"); len(rows) != 1 || rows[0].Outcome != "failure" {
			t.Errorf("run.command.exec rows = %v, want one failure row", rows)
		}
	})
}

// TestRunExecSuperAdminSkipsTheGovernanceDoor: a super admin's command into a run they may enter passes the
// governance door, as sshAuth exempts them, unless WARDYN_GOVERN_ADMIN_RUNS governs their runs and the run is not
// operator-owned (adminDoorExempt).
func TestRunExecSuperAdminSkipsTheGovernanceDoor(t *testing.T) {
	ok := func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("", "", 0), nil }
	for _, tc := range []struct {
		name     string
		governed bool
		execs    int32
	}{
		{"exempt", false, 1},
		{"governed by WARDYN_GOVERN_ADMIN_RUNS", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExecHarness(t, ok)
			e.underProfile(types.GovernanceLimits{DenyInteractive: true})
			e.srv.cfg.GovernAdminRuns = tc.governed
			admin := ssoSession(t, execOwner, "owner@corp.example", oidc.RoleAdmin)
			w := doSSO(t, e.srv, http.MethodPost, "/api/v1/runs/"+e.id.String()+"/exec", admin, `{"command":"id"}`)
			if n := e.execs.Load(); n != tc.execs {
				t.Errorf("ExecStream was called %d times, want %d (POST /exec = %d %s)", n, tc.execs, w.Code, w.Body)
			}
			if tc.governed && (w.Code != http.StatusForbidden || wireReason(t, w) != string(authz.ReasonGovernanceUnit)) {
				t.Errorf("POST /exec = %d %s, want 403 %s", w.Code, w.Body, authz.ReasonGovernanceUnit)
			}
		})
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
	} else if strings.Contains(string(rows[0].Data), `"command"`) {
		t.Errorf("the refusal row names the command though nothing ran: %s", rows[0].Data)
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

// stallingTouchStore's TouchRun waits until its context ends, as an UPDATE behind a row another transaction holds
// does, and counts the touches asked.
type stallingTouchStore struct {
	*authzStore
	asked atomic.Int32
}

func (s *stallingTouchStore) TouchRun(ctx context.Context, _ uuid.UUID) error {
	s.asked.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// TestRunExecTimeoutOutlastsAStalledTouch: an idle-clock touch that never answers does not hold a command past its
// timeout: the answer is timed_out within a bound, the runner's stream is closed, and the command's SSH channel slot
// is free again. Catches: the first touch made before collectExec, on a context the timeout does not reach.
func TestRunExecTimeoutOutlastsAStalledTouch(t *testing.T) {
	closed := &atomic.Bool{}
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return hangingExecSession(closed), nil })
	stall := &stallingTouchStore{authzStore: e.ast}
	e.srv.cfg.Store = stall
	e.srv.cfg.SSHMaxSessionsPerRun = 1
	res := execResult(t, e.execWithin(t, `{"command":"sleep 3600","timeout_sec":1}`, 5*time.Second))
	if !res.TimedOut || res.ExitCode != -1 {
		t.Errorf("result = %+v, want timed_out and exit_code -1", res)
	}
	if !closed.Load() {
		t.Error("the runner's exec stream was not closed on the timeout")
	}
	if stall.asked.Load() == 0 {
		t.Error("the run was never touched, so the stall was never met")
	}
	if !e.srv.sshAcquireSession(e.id) {
		t.Error("the command did not give its SSH channel slot back")
	}
	e.srv.sshReleaseSession(e.id)
}

// startLockFree reports whether run id's start lock is free now.
func startLockFree(srv *Server, id uuid.UUID) bool {
	_, unlock, ok := srv.tryLock(context.Background(), db.RunStartLockClass, id.String())
	if ok {
		unlock()
	}
	return ok
}

// startLockFreeWithin polls run id's start lock for up to d and reports whether it was ever free.
func startLockFreeWithin(srv *Server, id uuid.UUID, d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if startLockFree(srv, id) {
			return true
		}
	}
	return false
}

// TestRunExecHoldsTheStartLockOnlyToStart: the run's start lock is held while ExecStream starts the command, so no
// end lands between the last fresh read and the start, and is free once ExecStream has returned, while the command
// still runs, so an end never waits out a command. The command ends only when the test closes its streams.
// Catches: no lock held through ExecStream, and the lock held until the handler returns.
func TestRunExecHoldsTheStartLockOnlyToStart(t *testing.T) {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	inExec, startExec := make(chan struct{}), make(chan struct{})
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
		close(inExec)
		<-startExec
		return &runner.ExecSession{Stdout: outR, Stderr: errR, Wait: func() (int, error) { return 0, nil },
			Close: func() error { _ = outW.Close(); _ = errW.Close(); return nil }}, nil
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.exec(t, `{"command":"sleep 3600"}`) }()
	<-inExec
	if startLockFree(e.srv, e.id) {
		t.Error("the run's start lock was free while ExecStream was starting the command")
	}
	close(startExec)
	if !startLockFreeWithin(e.srv, e.id, 10*time.Second) {
		t.Error("the run's start lock was still held 10 s into a running command")
	}
	_ = outW.Close()
	_ = errW.Close()
	if res := execResult(t, <-done); res.ExitCode != 0 || res.TimedOut {
		t.Errorf("result = %+v, want exit 0 once the test ended the command", res)
	}
}

// TestRunExecCutsOffAStartPastItsBudget: a runner that has not started the command by execStartBudget has the
// run's start lock taken back from it, so the run's end does not wait on it, and the session it hands back late is
// closed and refused with 502 run_exec_failed. Catches: no start budget, and a late session run anyway.
func TestRunExecCutsOffAStartPastItsBudget(t *testing.T) {
	defer func(b time.Duration) { execStartBudget = b }(execStartBudget)
	execStartBudget = 50 * time.Millisecond
	closed := &atomic.Bool{}
	inExec, startExec := make(chan struct{}), make(chan struct{})
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
		close(inExec)
		<-startExec
		return hangingExecSession(closed), nil
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.exec(t, `{"command":"id","timeout_sec":1}`) }()
	<-inExec
	free := startLockFreeWithin(e.srv, e.id, 10*time.Second)
	close(startExec)
	w := <-done
	if !free {
		t.Error("the run's start lock was still held 10 s into a start past its budget")
	}
	if w.Code != http.StatusBadGateway || wireReason(t, w) != reasonRunExecFailed {
		t.Errorf("POST /exec = %d %s, want 502 %s", w.Code, w.Body, reasonRunExecFailed)
	}
	if !closed.Load() {
		t.Error("the session the runner handed back after the budget was not closed")
	}
}

// endsAtTheLock is a Locker whose first wait for key runs end first: an end that held the run's operation lock
// (lockRunEnd) writes the terminal state before a waiting door gets the lock. asked counts the waits for key.
type endsAtTheLock struct {
	db.Locker
	key   db.LockKey
	end   func()
	once  sync.Once
	asked atomic.Int32
}

func (l *endsAtTheLock) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	if k == l.key {
		l.asked.Add(1)
		l.once.Do(l.end)
	}
	return l.Locker.Lock(ctx, k, wait)
}

// lockEndsRun makes srv's first wait for run id's operation lock run end first.
func lockEndsRun(srv *Server, id uuid.UUID, end func()) *endsAtTheLock {
	l := &endsAtTheLock{Locker: db.NewLocalLocker(), key: db.NewLockKey(db.RunOpLockClass, id.String()), end: end}
	srv.locks.override = l
	return l
}

// startEndsRun makes srv's first wait for run id's start lock run end first: an end that takes no operation lock (a
// run with no computer folder pass back, a kill) lands just before a command's last read.
func startEndsRun(srv *Server, id uuid.UUID, end func()) *endsAtTheLock {
	l := &endsAtTheLock{Locker: db.NewLocalLocker(), key: runStartKey(id), end: end}
	srv.locks.override = l
	return l
}

// endRun applies end to the run ast holds, as an end's terminal write does.
func endRun(ast *authzStore, id uuid.UUID, end func(*types.AgentRun)) {
	ast.mu.Lock()
	defer ast.mu.Unlock()
	run := ast.runs[id]
	end(&run)
	ast.runs[id] = run
}

// endsAtTheStamp is a store with a run pauser whose presence stamp runs end first, as an end that takes no lock (a
// run with no computer folder pass back to wait for) may land then, and answers as StampRunActive answers for a run
// that has ended: false, nil.
type endsAtTheStamp struct {
	store.Store
	store.RunPauser // nil: a stamp of a run that is not paused reaches only StampRunActive
	end             func()
}

func (s *endsAtTheStamp) StampRunActive(context.Context, uuid.UUID) (bool, error) {
	s.end()
	return false, nil
}

// TestRunExecRefusesARunThatEnded: a command into a run that ended is refused with the ended run's words and never
// reaches the runner, whenever the end landed: before the first read (refused without waiting for any lock), while
// the command waited for the run's operation lock to thaw it (an end that keeps the sandbox for its computer folder's
// pass back holds it), during the thaw, where the stamp answers false, nil, or just before its last read, under the
// start lock, by an end that takes no operation lock. Catches: the first read dropped, and no read under the start
// lock.
func TestRunExecRefusesARunThatEnded(t *testing.T) {
	ok := func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("", "", 0), nil }
	completed := func(r *types.AgentRun) { r.State = types.RunCompleted }
	lost := time.Now()
	for _, tc := range []struct {
		name, when string // when: read, lock or thaw
		end        func(*types.AgentRun)
		want       string
	}{
		{"completed before the first read", "read", completed, fmt.Sprintf(execLiveRunWords.notRunning, types.RunCompleted)},
		{"completed at its lock, its sandbox kept", "lock", completed, fmt.Sprintf(execLiveRunWords.notRunning, types.RunCompleted)},
		{"its sandbox ref cleared at its lock", "lock", func(r *types.AgentRun) { r.SandboxRef = "" }, fmt.Sprintf(execLiveRunWords.notRunning, types.RunRunning)},
		{"kept at its lock", "lock", func(r *types.AgentRun) { r.LostAt = &lost }, execLiveRunWords.ended},
		{"completed during its thaw", "thaw", completed, fmt.Sprintf(execLiveRunWords.notRunning, types.RunCompleted)},
		{"completed at its start lock, by an end that takes no run lock", "start", completed, fmt.Sprintf(execLiveRunWords.notRunning, types.RunCompleted)},
		{"kept at its start lock", "start", func(r *types.AgentRun) { r.LostAt = &lost }, execLiveRunWords.ended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExecHarness(t, ok)
			end := func() { endRun(e.ast, e.id, tc.end) }
			atLock := func() {}
			switch tc.when {
			case "read":
				end()
			case "lock":
				atLock = end
			case "thaw":
				e.srv.cfg.Store = &endsAtTheStamp{Store: e.ast, end: end}
			}
			l := lockEndsRun(e.srv, e.id, atLock)
			if tc.when == "start" {
				l = startEndsRun(e.srv, e.id, end)
			}
			w := e.exec(t, `{"command":"id"}`)
			if w.Code != http.StatusConflict || wireReason(t, w) != reasonRunExecUnavailable || execError(t, w) != tc.want {
				t.Errorf("POST /exec = %d %s, want 409 %s saying %q", w.Code, w.Body, reasonRunExecUnavailable, tc.want)
			}
			if n := e.execs.Load(); n != 0 {
				t.Errorf("ExecStream was called %d times for a run that ended", n)
			}
			if n := l.asked.Load(); (n > 0) != (tc.when != "read") {
				t.Errorf("the run's lock was asked %d times; want it asked only after a first read that found the run live", n)
			}
		})
	}
}

// TestRunExecRefusesWhenTheRunLockCannotBeTaken: a run lock that cannot be taken refuses the command with the
// lock's own words, and nothing reaches the runner. Catches: a command admitted without the lock.
func TestRunExecRefusesWhenTheRunLockCannotBeTaken(t *testing.T) {
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("", "", 0), nil })
	e.srv.locks.override = exhaustedPool()
	w := e.exec(t, `{"command":"id"}`)
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonRunExecUnavailable || execError(t, w) != lockUnavailableMsg {
		t.Errorf("POST /exec = %d %s, want 409 %s saying %q", w.Code, w.Body, reasonRunExecUnavailable, lockUnavailableMsg)
	}
	if n := e.execs.Load(); n != 0 {
		t.Errorf("ExecStream was called %d times without the run's lock", n)
	}
}

// refusesStarts is the in-process locker with every run start lock refused, as an exhausted lock pool refuses it.
type refusesStarts struct{ db.Locker }

func (l refusesStarts) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	if k.Class == db.RunStartLockClass {
		return ctx, nil, db.ErrLockNoCapacity
	}
	return l.Locker.Lock(ctx, k, wait)
}

// TestRunExecRefusesWhenTheStartLockCannotBeTaken: a start lock that cannot be taken refuses the command with the
// lock's own words and nothing reaches the runner, while an end of the same run still lands, unfenced. Catches: a
// command started without the fence, and an end the fence refused.
func TestRunExecRefusesWhenTheStartLockCannotBeTaken(t *testing.T) {
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("", "", 0), nil })
	e.srv.locks.override = refusesStarts{db.NewLocalLocker()}
	w := e.exec(t, `{"command":"id"}`)
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonRunExecUnavailable || execError(t, w) != lockUnavailableMsg {
		t.Errorf("POST /exec = %d %s, want 409 %s saying %q", w.Code, w.Body, reasonRunExecUnavailable, lockUnavailableMsg)
	}
	if n := e.execs.Load(); n != 0 {
		t.Errorf("ExecStream was called %d times without the run's start lock", n)
	}
	if ok, err := e.srv.casRunState(context.Background(), e.id, types.RunRunning, types.RunCompleted); !ok || err != nil {
		t.Errorf("an end whose fence was refused: applied %v, %v; want it written anyway", ok, err)
	}
}

// casAfterTheLastRead is the store of a run that ends with no run lock (no computer folder pass back) right after a
// command start's last read: the read under the run's start lock sets a terminal CAS going, then gives it time to
// land.
type casAfterTheLastRead struct {
	store.Store
	srv    *Server
	once   sync.Once
	landed chan bool
}

func (s *casAfterTheLastRead) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	run, err := s.Store.GetRun(ctx, id)
	if slices.ContainsFunc(db.HeldLocks(ctx), func(k db.LockKey) bool { return k.Class == db.RunStartLockClass }) {
		s.once.Do(func() {
			go func() {
				ok, _ := s.srv.casRunState(context.Background(), id, types.RunRunning, types.RunCompleted)
				s.landed <- ok
			}()
			time.Sleep(50 * time.Millisecond)
		})
	}
	return run, err
}

// TestRunExecAnEndAfterTheLastReadWaitsForTheStart: a background run with no computer folder ends (its terminal
// CAS) right after a command's last read. The end waits for the start, so the runner starts the command in a run that
// is still RUNNING, and the end lands once the start has returned. Catches: a terminal write that does not take the
// run's start lock, so a command starts after the run ended.
func TestRunExecAnEndAfterTheLastReadWaitsForTheStart(t *testing.T) {
	var e *execHarness
	var atStart types.RunState
	e = newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
		run, _ := e.ast.GetRun(context.Background(), e.id)
		atStart = run.State
		return fakeExecSession("", "", 0), nil
	})
	e.background()
	cs := &casAfterTheLastRead{Store: e.ast, srv: e.srv, landed: make(chan bool, 1)}
	e.srv.cfg.Store = cs
	if w := e.exec(t, `{"command":"id"}`); w.Code != http.StatusOK || atStart != types.RunRunning {
		t.Errorf("POST /exec = %d %s with the run %s at the start; want 200 with the run still RUNNING when the command started",
			w.Code, w.Body, atStart)
	}
	select {
	case ok := <-cs.landed:
		if !ok {
			t.Error("the end did not land once the command had started")
		}
	case <-time.After(10 * time.Second):
		t.Error("the end was still waiting 10 s after the command started")
	}
}

// losesStarts is the in-process locker whose run start locks a test can lose, as a Postgres session's lock is lost
// with its connection: lose ends the last start hold's context with db.ErrLockLost, and atTake loses each one as it
// is taken.
type losesStarts struct {
	db.Locker
	atTake bool
	mu     sync.Mutex
	last   context.CancelCauseFunc
}

func (l *losesStarts) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	lctx, release, err := l.Locker.Lock(ctx, k, wait)
	if err != nil || k.Class != db.RunStartLockClass {
		return lctx, release, err
	}
	hctx, lose := context.WithCancelCause(lctx)
	l.mu.Lock()
	l.last = lose
	l.mu.Unlock()
	if l.atTake {
		lose(db.ErrLockLost)
	}
	return hctx, release, nil
}

func (l *losesStarts) lose() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last(db.ErrLockLost)
}

// TestRunExecRefusesAStartWhoseLockIsLost: a start lock lost after the last read, before ExecStream, during it, or
// found lost on the lock's own connection once ExecStream has returned, refuses the command with 502
// run_exec_failed; no command is started after a loss the server saw, and a session the runner hands back is closed.
// Catches: the loss ignored before the start, during it, and after it.
func TestRunExecRefusesAStartWhoseLockIsLost(t *testing.T) {
	for _, when := range []string{"before", "during", "after"} {
		t.Run(when, func(t *testing.T) {
			closed := &atomic.Bool{}
			l := &losesStarts{Locker: db.NewLocalLocker(), atTake: when == "before"}
			e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) {
				if when == "during" {
					l.lose()
				}
				return hangingExecSession(closed), nil
			})
			e.srv.locks.override = l
			if when == "after" {
				e.srv.locks.held = func(ctx context.Context) error {
					if slices.ContainsFunc(db.HeldLocks(ctx), func(k db.LockKey) bool { return k.Class == db.RunStartLockClass }) {
						return db.ErrLockLost
					}
					return db.LockHeld(ctx)
				}
			}
			w := e.exec(t, `{"command":"id","timeout_sec":1}`)
			if w.Code != http.StatusBadGateway || wireReason(t, w) != reasonRunExecFailed {
				t.Errorf("POST /exec = %d %s, want 502 %s", w.Code, w.Body, reasonRunExecFailed)
			}
			if n := e.execs.Load(); (n == 0) != (when == "before") {
				t.Errorf("ExecStream was called %d times; want none exactly when the lock was lost before the start", n)
			}
			if when != "before" && !closed.Load() {
				t.Error("the session the runner handed back after the loss was not closed")
			}
		})
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

// TestRunExecCallerLeftIsAFailure: a caller who leaves while the command runs gets no success row with a
// made-up exit code: the row is a failure saying the caller left, and the runner's stream is closed.
func TestRunExecCallerLeftIsAFailure(t *testing.T) {
	closed := &atomic.Bool{}
	e := newExecHarness(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return hangingExecSession(closed), nil })
	owner := ssoSession(t, execOwner, "owner@corp.example", oidc.RoleUser)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		doSSOCtx(t, e.srv, ctx, http.MethodPost, "/api/v1/runs/"+e.id.String()+"/exec", owner, `{"command":"sleep 3600","timeout_sec":300}`)
	}()
	for deadline := time.Now().Add(10 * time.Second); e.execs.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the command never reached the runner")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("POST /exec did not end after its caller left")
	}
	if !closed.Load() {
		t.Error("the runner's exec stream was not closed when the caller left")
	}
	rows := execRows(e.h.audit.snapshot(), "run.command.exec")
	if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), runExecCallerLeft) {
		t.Errorf("run.command.exec rows = %v, want one failure saying the caller left", rows)
	}
}

// TestRunExecAuditsTheCommandCapped: a long command runs whole, and its audit row keeps the first
// runExecAuditCommandCap bytes of it, with the whole command's length and SHA-256.
func TestRunExecAuditsTheCommandCapped(t *testing.T) {
	var got []string
	e := newExecHarness(t, func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		got = spec.Argv
		return fakeExecSession("", "", 0), nil
	})
	long := "echo " + strings.Repeat("y", 100<<10)
	execResult(t, e.execWithin(t, `{"command":"`+long+`"}`, 10*time.Second))
	if len(got) == 0 || got[len(got)-1] != long {
		t.Fatal("the runner did not get the whole command")
	}
	rows := execRows(e.h.audit.snapshot(), "run.command.exec")
	if len(rows) != 1 {
		t.Fatalf("run.command.exec rows = %d, want one", len(rows))
	}
	var data struct {
		Command       string `json:"command"`
		CommandBytes  int    `json:"command_bytes"`
		CommandSHA256 string `json:"command_sha256"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256([]byte(long)); data.CommandBytes != len(long) || data.CommandSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("command_bytes %d, command_sha256 %q; want %d and the whole command's SHA-256", data.CommandBytes, data.CommandSHA256, len(long))
	}
	if len(data.Command) > runExecAuditCommandCap+len(store.AuditTargetTruncatedMarker) || !strings.HasPrefix(data.Command, "echo yyy") ||
		!strings.HasSuffix(data.Command, store.AuditTargetTruncatedMarker) {
		t.Errorf("audited command is %d bytes (%.20q…), want at most %d and marked cut", len(data.Command), data.Command, runExecAuditCommandCap)
	}
}
