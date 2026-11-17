// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

// errSSHChannelClosed is the cause a door's context ends with when its channel or connection ends while the daemon
// runs: a session channel's context when its request stream ends while the connection's context is still live,
// and the connection's context when the connection ends (the client closed it or it dropped, or the gateway
// closed one that stopped answering or lost its key, sshProbeConn).
var errSSHChannelClosed = errors.New("the SSH channel or its connection closed")

// sshClientClosedFirst reports whether ctx, a door's context (a session channel's, or the connection's for a port
// forward), ended because its client closed the channel or its connection (errSSHChannelClosed), not because the
// daemon stopped, which ends the connection's context first and so every door's with that context's own cause.
func sshClientClosedFirst(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errSSHChannelClosed)
}

// sshStartCutByClient reports whether err, a door's start error, is the start lock wait or the start that its
// client's own close cut (sshClientClosedFirst), not one of the fence's own refusals (its wait's bound, the start
// budget, a lost lock) nor a daemon stop.
func sshStartCutByClient(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) && sshClientClosedFirst(ctx)
}

// sshStartError is a door's start error as its failure row records it: a start its client's close cut names that
// close (errSSHChannelClosed), where the start lock's busy sentence would name a cause that was not there; a start
// the fence refused reads the fence's own sentence; anything else is the step that failed (sftp, exec and forward
// pass theirs), since a runner's error text can name a socket, a path or a host. The cause goes to the server log.
func sshStartError(ctx context.Context, runID uuid.UUID, step string, err error) string {
	switch {
	case sshStartCutByClient(ctx, err):
		return errSSHChannelClosed.Error()
	case errors.As(err, new(startUnfenced)):
		return lockUnavailableMsg
	case errors.Is(err, errStartTooSlow):
		return startTooSlowMsg
	case errors.Is(err, errStartLockLost):
		return startLockLostMsg
	}
	return auditStep(ctx, step, err, slog.String("run_id", runID.String()))
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
		outcome, data := "failure", map[string]any{}
		if !sshStartCutByClient(ctx, err) {
			data["error"] = sshStartError(ctx, runID, auditStepSFTPStart, err)
		} else {
			// A start its client's own close cut is the client's end, as a close under the running process is
			// (bridgeOutcome): a success that names no error, with nothing moved. Only a wait the channel's end
			// cut reads so: the fence's own refusals (its wait's bound, the start budget, a lost lock) are never
			// that close, so they stay failures when the client happens to close too.
			outcome, data = "success", map[string]any{"bytes": 0, "bytes_in": 0, "bytes_out": 0}
		}
		s.recordStreamAuditDetached(ctx, run.SandboxRef, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sftp.transfer",
			runID.String(), outcome, mustJSON(data)))
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
	closed bool  // the client's close had ended the bridge's context when the process's output ended (sshClientClosedFirst), before the bridge waited for its stderr; a daemon stop is not the client's
}

// noteBridgeEnd keeps how sess ended in end. ctx is the context the bridge runs
// on. end.closed is read when the process's output ends (its Stdout's first read
// error), before the bridge waits for the process's stderr, so a client that
// closes during that wait does not turn a failed process into a client's end.
// It is read again as Wait returns when Wait erred (a Wait bound to the ended
// context reads no exit, so the close is all there is to judge by), when there
// is no Stdout, or when the copy stopped without a read error. end.err is Wait's
// own error.
func noteBridgeEnd(ctx context.Context, sess *runner.ExecSession, end *bridgeEnd) *runner.ExecSession {
	if sess.Wait == nil {
		return sess
	}
	noted := *sess
	var out *endNoteReader
	if sess.Stdout != nil {
		out = &endNoteReader{r: sess.Stdout, ctx: ctx, end: end}
		noted.Stdout = out
	}
	noted.Wait = func() (int, error) {
		code, err := sess.Wait()
		end.err = err
		if err != nil || out == nil || !out.noted {
			end.closed = sshClientClosedFirst(ctx)
		}
		return code, err
	}
	return &noted
}

// endNoteReader notes in end.closed, at its first read error (EOF or not),
// whether the client's close had ended ctx. sshBridgeExecSession copies Stdout
// until the copy ends and then calls Wait on one goroutine, in that order: no
// lock is needed.
type endNoteReader struct {
	r     io.Reader
	ctx   context.Context
	end   *bridgeEnd
	noted bool
}

func (e *endNoteReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && !e.noted {
		e.end.closed, e.noted = sshClientClosedFirst(e.ctx), true
	}
	return n, err
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
// and not a failed transfer, so the row stays a success.
// A daemon stop that ends the bridge's context under the process is not the client's end: the process it cut
// left no exit, so the row is a failure that says so (sshClientClosedFirst tells the two apart).
// ssh.exec keeps its own rule: the command's exit is the person's, recorded as exit on a success.
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
