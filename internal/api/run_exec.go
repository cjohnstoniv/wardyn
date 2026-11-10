// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// POST /runs/{id}/exec runs one command in a running run and answers with its output: the SSH
// gateway's exec channel (bridgeSSHExec) over plain HTTP, and no wider. Before the runner is called it
// checks, in order, who may enter the run (getRunForEntry), the profile's deny_interactive (as SSH
// authentication does), the run's data_out rule (as an SSH exec channel does), the run's state
// (sshFreshRun) and the run's SSH channel cap, which a command shares with SSH (sshAcquireSession). The command runs as an SSH exec runs it (sshExecArgv), with no TTY and no added
// environment, and is never recorded: its run.command.exec row names the command and its exit code, never
// its output.

const (
	// runExecDefaultTimeout is a request's timeout when it names none; runExecMaxTimeoutSec the most it may name.
	runExecDefaultTimeout = 60 * time.Second
	runExecMaxTimeoutSec  = 300
	// runExecStreamCap is how much of each stream the reply keeps. Reading never stops there: a stream
	// nobody reads blocks the command (runner.ExecSession's streaming contract).
	runExecStreamCap = 1 << 20
	// runExecDataOutMsg is sshDataOutRefused's sentence for this door.
	runExecDataOutMsg = "This run's data_out rule (%s) keeps content in, so commands are off; the terminal still works."
	// runExecSandboxGoneMsg answers a run whose sandbox went away between the state check and the exec.
	runExecSandboxGoneMsg = "The run's sandbox is gone; the run is ending."
	// runExecBusyMsg answers a run already holding its most SSH channels and commands at once.
	runExecBusyMsg = "This run already has %d SSH channels or commands open, the most it may; try again when one ends."
	// runExecFailedMsg answers a runner that could not start the command or lost it; the cause goes to the log.
	runExecFailedMsg = "The runner could not run the command, or lost it before it ended."
)

func (s *Server) handleRunExec(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunForEntry(w, r, id)
	if !ok {
		return
	}
	// sshAuth's deny_interactive door: a super admin is past getRunForEntry only on their own run or
	// an operator-owned one, so the exemption cannot reach a person's run.
	if (!s.isOperator(r.Context()) || !s.adminDoorExempt(run)) && s.refuseInteractiveAttach(w, r, run) {
		return
	}
	var req client.ExecRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	timeout, msg := runExecTimeout(req)
	if msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonRunExecInvalid, msg)
		return
	}
	if s.refuseExecDataOut(w, r, run) {
		return
	}
	ctx := r.Context()
	at, actor := actorFromRequest(r)
	fresh, msg := s.sshFreshRun(ctx, run.ID, actor)
	if msg != "" {
		writeErrorReason(w, http.StatusConflict, reasonRunExecUnavailable, msg)
		return
	}
	// One of the run's SSH channel slots, the same per-run cap an SSH exec draws from.
	if !s.sshAcquireSession(run.ID) {
		limit := s.cfg.SSHMaxSessionsPerRun
		s.auditRunExec(ctx, fresh, at, actor, "failure", map[string]any{"command": req.Command, "error": fmt.Sprintf("%s (max %d)", sshCapReasonChannels, limit)})
		writeErrorReason(w, http.StatusTooManyRequests, reasonRunExecBusy, fmt.Sprintf(runExecBusyMsg, limit))
		return
	}
	defer s.sshReleaseSession(run.ID)
	s.runExec(w, r, fresh, req.Command, timeout)
}

// runExec runs command in fresh and answers its result. While it runs, the run's idle clock is kept
// reset (TouchRun and attachKeepalive), as for an SSH exec, so auto_stop does not end a long command.
func (s *Server) runExec(w http.ResponseWriter, r *http.Request, fresh types.AgentRun, command string, timeout time.Duration) {
	ctx := r.Context()
	at, actor := actorFromRequest(r)
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sess, err := s.cfg.Runner.ExecStream(execCtx, fresh.SandboxRef, runner.ExecSpec{Argv: sshExecArgv(command)})
	if err == nil && sess == nil {
		err = errors.New("the runner returned no exec session")
	}
	if err != nil {
		s.auditRunExec(ctx, fresh, at, actor, "failure", map[string]any{"command": command, "error": err.Error()})
		writeExecStartError(w, r, err)
		return
	}
	keepCtx, stopKeep := context.WithCancel(ctx)
	defer stopKeep()
	_ = s.cfg.Store.TouchRun(keepCtx, fresh.ID)
	go s.attachKeepalive(keepCtx, fresh.ID)
	res, err := collectExec(execCtx, sess)
	if err != nil {
		s.auditRunExec(ctx, fresh, at, actor, "failure", map[string]any{"command": command, "error": err.Error()})
		writeExecStartError(w, r, err)
		return
	}
	data, outcome := map[string]any{"command": command, "exit": res.ExitCode}, "success"
	if res.TimedOut {
		data["error"], outcome = fmt.Sprintf("timed out after %s", timeout), "failure"
	}
	s.auditRunExec(ctx, fresh, at, actor, outcome, data)
	writeJSON(w, http.StatusOK, res)
}

// runExecTimeout is the request's timeout, or the sentence refusing the request.
func runExecTimeout(req client.ExecRequest) (time.Duration, string) {
	if strings.TrimSpace(req.Command) == "" {
		return 0, "command is required"
	}
	switch {
	case req.TimeoutSec == 0:
		return runExecDefaultTimeout, ""
	case req.TimeoutSec < 1 || req.TimeoutSec > runExecMaxTimeoutSec:
		return 0, fmt.Sprintf("timeout_sec must be from 1 to %d", runExecMaxTimeoutSec)
	}
	return time.Duration(req.TimeoutSec) * time.Second, ""
}

// refuseExecDataOut is an SSH exec channel's data_out gate (sshDataOut, sshDataOutRefused) for this
// door: a rule that cannot be read is blocked, and any rule but allowed refuses the command with the
// run.data_out.deny row SSH writes. True when it has answered.
func (s *Server) refuseExecDataOut(w http.ResponseWriter, r *http.Request, run types.AgentRun) bool {
	ctx := r.Context()
	rule, _, err := s.runDataOutRule(ctx, run.ID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: a run's data_out rule is unreadable; its commands are refused", "run_id", run.ID, "err", err)
		rule = types.TransferBlocked
	}
	if rule.DataOut() == types.TransferAllowed {
		return false
	}
	at, actor := actorFromRequest(r)
	s.auditDataOutDenied(ctx, run.ID, at, actor, "exec", rule)
	writeErrorReason(w, http.StatusConflict, reasonDataOutBlocked, fmt.Sprintf(runExecDataOutMsg, rule))
	return true
}

// writeExecStartError answers a command the runner did not start, or lost before it ended.
func writeExecStartError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, runner.ErrExecStreamUnsupported):
		writeErrorReason(w, http.StatusNotImplemented, reasonRunInspectExecStreamUnsupported, sshExecStreamUnsupportedMsg)
	case errors.Is(err, runner.ErrSandboxGone):
		writeErrorReason(w, http.StatusConflict, reasonRunExecUnavailable, runExecSandboxGoneMsg)
	default:
		writeErrorReason(w, http.StatusBadGateway, reasonRunExecFailed, loggedMsg(r.Context(), runExecFailedMsg, err))
	}
}

// auditRunExec writes the command's run.command.exec row: the command and its exit code, or the error, never
// output. It outlives a caller who left before the command ended.
func (s *Server) auditRunExec(ctx context.Context, run types.AgentRun, at types.ActorType, actor, outcome string, data map[string]any) {
	s.recordStreamAudit(context.WithoutCancel(ctx), run.SandboxRef, s.auditEvent(&run.ID, at, actor, "run.command.exec",
		run.ID.String(), outcome, mustJSON(data)))
}

// collectExec reads both of sess's streams to their end, keeping the first runExecStreamCap bytes of
// each, and then its exit code. When ctx ends first (the timeout, or a caller who left) the session is
// closed, which ends both streams, and the exit code is -1. Otherwise a stream that broke, or a Wait
// that gave no exit code, is the error: the runner lost the command (a k8s exec whose connection failed
// after ExecStream returned ends its streams that way), and no exit code is made up for it.
func collectExec(ctx context.Context, sess *runner.ExecSession) (client.ExecResult, error) {
	// Close is what ends a stream no context reaches (a docker exec's hijacked connection), as
	// sshBridgeExecSession does on a dropped connection.
	stop := context.AfterFunc(ctx, func() {
		if sess.Close != nil {
			_ = sess.Close()
		}
	})
	defer stop()
	defer func() {
		if sess.Close != nil {
			_ = sess.Close()
		}
	}()
	if sess.Stdin != nil {
		_ = sess.Stdin.Close() // no input: a command reading stdin sees its end
	}
	stdout, stderr := &execCapture{max: runExecStreamCap}, &execCapture{max: runExecStreamCap}
	var wg sync.WaitGroup
	var errErr, outErr error
	if sess.Stderr != nil {
		// Drained beside Stdout, never after it: one demux feeds both (drainExecStderr).
		wg.Go(func() { _, errErr = io.Copy(stderr, sess.Stderr) })
	}
	if sess.Stdout != nil {
		_, outErr = io.Copy(stdout, sess.Stdout)
	}
	wg.Wait()
	res := client.ExecResult{Stdout: string(stdout.buf), Stderr: string(stderr.buf), Truncated: stdout.dropped || stderr.dropped}
	if ctx.Err() != nil {
		res.ExitCode, res.TimedOut = -1, errors.Is(ctx.Err(), context.DeadlineExceeded)
		return res, nil
	}
	if err := errors.Join(outErr, errErr); err != nil {
		return client.ExecResult{}, fmt.Errorf("the command's output stream broke: %w", err)
	}
	if sess.Wait == nil {
		return client.ExecResult{}, errors.New("the runner gave no exit code")
	}
	code, err := sess.Wait()
	if err != nil {
		if ctx.Err() != nil {
			res.ExitCode, res.TimedOut = -1, errors.Is(ctx.Err(), context.DeadlineExceeded)
			return res, nil
		}
		return client.ExecResult{}, fmt.Errorf("the runner lost the command: %w", err)
	}
	res.ExitCode = clampExitCode(code)
	return res, nil
}

// execCapture keeps the first max bytes written to it and notes that more came. It never refuses a
// write, so the stream behind it is read to its end.
type execCapture struct {
	buf     []byte
	max     int
	dropped bool
}

func (c *execCapture) Write(p []byte) (int, error) {
	n := min(len(p), c.max-len(c.buf))
	c.buf = append(c.buf, p[:n]...)
	if n < len(p) {
		c.dropped = true
	}
	return len(p), nil
}
