// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sftpServerPath is the sandbox binary the sftp subsystem execs — the
// documented BYOI image contract (docs/SSH.md).
const sftpServerPath = "/usr/lib/openssh/sftp-server"

// sftpServerPathMacOS is where a macOS VM keeps it: the system's own, so the base needs no install.
const sftpServerPathMacOS = "/usr/libexec/sftp-server"

// sftpServerPathFor is the sftp-server path of a run's machine kind: the Linux path for a container
// and a Linux VM (its image installs openssh-sftp-server), the system's for a macOS VM.
func sftpServerPathFor(kind types.MachineKind) string {
	if kind.IsMacOSVM() {
		return sftpServerPathMacOS
	}
	return sftpServerPath
}

// execSFTPServer starts the sandbox's own sftp-server (plus args) and, on a
// launch failure, returns the message the client sees: an offline runner or a
// gone sandbox is named as such, ErrExecStreamUnsupported maps to the shared
// honest message every primitive here uses, and any other launch failure is
// read as a BYOI image without the binary and names the image contract (never
// a hang). fenced starts it as a person's channel does, under the run's start lock
// on the run as it stands there (execFresh); a computer folder's pass, which an
// end runs on a run that is no longer live, is never fenced.
func (s *Server) execSFTPServer(ctx context.Context, run types.AgentRun, fenced bool, args ...string) (*runner.ExecSession, string, error) {
	spec := runner.ExecSpec{Argv: append([]string{sftpServerPathFor(run.MachineKind), "-e"}, args...)}
	var (
		sess *runner.ExecSession
		err  error
	)
	if fenced {
		sess, err = s.execFresh(ctx, run.ID, sshLiveRunWords, spec)
	} else {
		sess, err = s.cfg.Runner.ExecStream(ctx, run.SandboxRef, spec)
	}
	if err == nil {
		return sess, "", nil
	}
	reason := sshExecStreamErrorMessage(err)
	switch {
	case errors.Is(err, runner.ErrExecStreamUnsupported):
	case errors.Is(err, runner.ErrRunnerOffline):
		// The sentence already says what the runner error says; repeating it reads twice.
		reason = "sftp unavailable: the runner for this sandbox is offline; retry when it reconnects"
	case errors.Is(err, runner.ErrSandboxGone):
		reason = "sftp unavailable: the sandbox is gone"
	case startCut(err):
		reason = "sftp unavailable: " + err.Error()
	default:
		reason = strings.Replace(sftpServerMissing, sftpServerPath, sftpServerPathFor(run.MachineKind), 1) + " (" + err.Error() + ")"
	}
	return nil, reason, err
}

// sftpServerMissing is the image contract a missing sftp-server breaks.
const sftpServerMissing = "sftp unavailable: the sandbox image has no " + sftpServerPath

// withSFTPLaunchNote tells the client the image contract when the sftp-server sess runs ends having
// sent nothing, with the shell's exit for a binary it cannot run (126, 127) or a stream error: a
// substrate that starts the exec lazily (Kubernetes) reports a missing binary only after launch, so
// execSFTPServer's message never applies, and the client would see a bare exit 1. A sandbox torn
// down, or a session cancelled, before the server wrote a byte says nothing about the image.
func withSFTPLaunchNote(sess *runner.ExecSession, stderr io.Writer) *runner.ExecSession {
	if sess.Stdout == nil || sess.Wait == nil {
		return sess
	}
	out := &sftpOutput{r: sess.Stdout}
	noted := *sess
	noted.Stdout = out
	noted.Wait = func() (int, error) {
		code, err := sess.Wait()
		if out.n == 0 && (code == 126 || code == 127 || err != nil) && !sftpEndNotTheImage(err) {
			_, _ = fmt.Fprintln(stderr, sftpServerMissing)
		}
		return code, err
	}
	return &noted
}

// sftpEndNotTheImage is an end that a missing sftp-server cannot explain.
func sftpEndNotTheImage(err error) bool {
	return errors.Is(err, runner.ErrSandboxGone) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// sftpOutput counts what the sftp-server sent; sshBridgeExecSession reads it to its end before Wait.
type sftpOutput struct {
	r io.Reader
	n int64
}

func (o *sftpOutput) Read(p []byte) (int, error) {
	k, err := o.r.Read(p)
	o.n += int64(k)
	return k, err
}

// bridgeSSHSFTP runs the sandbox's own sftp-server as the subsystem's backing
// process — no SFTP protocol reimplementation. `-d %d` starts it in the
// sandbox user's home directory from its passwd entry, as sshd does, so
// relative paths resolve from $HOME; sftp-server stays in the image's working
// directory when it cannot enter that directory (#3549).
func (s *Server) bridgeSSHSFTP(ctx context.Context, runID uuid.UUID, principal string, channel ssh.Channel) {
	run, msg := s.sshFreshRun(ctx, runID, principal)
	if msg != "" {
		sendChannelError(channel, msg)
		return
	}
	sess, reason, err := s.execSFTPServer(ctx, run, true, "-d", "%d")
	if err != nil {
		s.recordStreamAudit(ctx, run.SandboxRef, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sftp.transfer",
			runID.String(), "failure", mustJSON(map[string]any{"error": err.Error()})))
		sendChannelError(channel, reason)
		return
	}
	var end bridgeEnd
	noted := noteBridgeEnd(ctx, withSFTPLaunchNote(sess, channel.Stderr()), &end)
	exit, bytesIn, bytesOut := s.sshBridgeExecSession(ctx, runID, principal, channel, noted, true)
	// BaseCtx, not ctx — see bridgeSSHExec's identical trailing-write FINDING
	// comment above (same shape, same connection-teardown race, same fix).
	// "bytes" keeps its pre-0.9 meaning (sandbox to client); bytes_in/bytes_out
	// are the explicit pair.
	extra := map[string]any{"bytes": bytesOut, "bytes_in": bytesIn, "bytes_out": bytesOut}
	outcome := bridgeOutcome(exit, end, "sftp-server", extra)
	s.recordStreamAudit(s.cfg.BaseCtx, run.SandboxRef, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sftp.transfer",
		runID.String(), outcome, mustJSON(extra)))
}

// bridgeEnd is how a bridge's process ended, kept from its Wait for
// bridgeOutcome: sshBridgeExecSession folds a Wait that erred to exit 1, which
// says nothing about the process.
type bridgeEnd struct {
	err    error // Wait's own error: no exit was read
	closed bool  // the session's context had ended by then: the client closed first
}

// noteBridgeEnd keeps how sess.Wait ended in end. ctx is the context the bridge
// runs on, read as Wait returns, before the bridge closes the channel itself.
func noteBridgeEnd(ctx context.Context, sess *runner.ExecSession, end *bridgeEnd) *runner.ExecSession {
	if sess.Wait == nil {
		return sess
	}
	noted := *sess
	noted.Wait = func() (int, error) {
		code, err := sess.Wait()
		end.err, end.closed = err, ctx.Err() != nil
		return code, err
	}
	return &noted
}

// bridgeOutcome is the audit outcome of a bridge whose process is the gateway's
// own transport (sftp-server, the forward relay), from the exit
// sshBridgeExecSession returned: a nonzero exit is a transfer that failed, such
// as a missing sftp-server a lazy substrate reports only after launch or a
// relay whose loopback target refused, so it is a failure with the exit in
// extra's error. A client that closed its channel or its connection before the
// process ended (most SFTP libraries close where OpenSSH half-closes and waits;
// an ssh -L ended with a connection still open) left no exit to read: the
// session is closed under the process and Wait errs, which is the client's end
// and not a failed transfer, so the row stays a success. ssh.exec keeps its own
// rule: the command's exit is the person's, recorded as exit on a success.
func bridgeOutcome(exit int, end bridgeEnd, process string, extra map[string]any) string {
	switch {
	case end.closed, end.err == nil && exit == 0:
		return "success"
	case end.err != nil:
		extra["error"] = process + " ended without an exit status"
	default:
		extra["error"] = fmt.Sprintf("%s exited %d", process, exit)
	}
	return "failure"
}
