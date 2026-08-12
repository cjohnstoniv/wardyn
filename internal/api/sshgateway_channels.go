// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Channel-level SSH gateway protocol handling: the "session" channel's
// request loop (pty-req/window-change/env/shell/exec/subsystem) and the
// bridges to the sandbox — Attach for shell, runner.ExecStream for
// exec/sftp/direct-tcpip. See sshgateway.go for connection lifecycle + auth.
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sftpServerPath is the sandbox binary the sftp subsystem execs — the
// documented BYOI image contract (docs/SSH.md).
const sftpServerPath = "/usr/lib/openssh/sftp-server"

// sshExecStreamUnsupportedMsg is what every ExecStream consumer in this file
// reports on runner.ErrExecStreamUnsupported: a clean channel error, never a
// hang, on a substrate with no exec-streaming primitive.
const sshExecStreamUnsupportedMsg = "this sandbox's runtime does not support SSH exec/sftp/forwarding"

// SSH connection-protocol request/channel-open payloads (RFC 4254 §6, §7.2),
// mirrored here with EXPORTED fields so ssh.Unmarshal (reflection-based,
// defined in x/crypto/ssh) can populate them from this package. Field order
// and types ARE the wire contract — do not reorder.
type sshPTYReqMsg struct {
	Term    string
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
	Modes   string
}

type sshWindowChangeMsg struct {
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
}

type sshEnvMsg struct {
	Name  string
	Value string
}

type sshExecReqMsg struct {
	Command string
}

type sshSubsystemMsg struct {
	Subsystem string
}

type sshDirectTCPIPMsg struct {
	DestHost string
	DestPort uint32
	OrigHost string
	OrigPort uint32
}

// sshEnvAllowed is the exec-path env allowlist: TERM and LC_*/LANG locale
// vars only, matching the docker driver's own hardcoded Attach env
// (runner/docker/session.go's attachShell) so exec's locale behaves the same
// as the shell path's. Everything else an "env" request offers is silently
// dropped — never forwarded into ExecSpec.Env.
func sshEnvAllowed(name string) bool {
	return name == "TERM" || name == "LANG" || strings.HasPrefix(name, "LC_")
}

// sshIsSandboxLoopback reports whether host names the sandbox's OWN loopback
// — the only destination direct-tcpip may ever reach. The sandbox's network
// namespace has no route anywhere else regardless (it cannot create egress —
// invariant 3), but this is validated up front so a non-loopback ask is
// refused with a clear reason instead of silently rewritten or left to fail
// inside the sandbox.
func sshIsSandboxLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sshExecStreamErrorMessage maps an ExecStream launch failure to the message
// a client sees: the shared "unsupported substrate" wording on
// ErrExecStreamUnsupported, the raw error otherwise.
func sshExecStreamErrorMessage(err error) string {
	if errors.Is(err, runner.ErrExecStreamUnsupported) {
		return sshExecStreamUnsupportedMsg
	}
	return "exec failed: " + err.Error()
}

// sendChannelError writes msg to the channel's stderr stream and reports a
// nonzero exit — the "clean channel error" every precondition failure and
// ExecStream/Attach LAUNCH failure in this file uses (bridgeSSHShell,
// bridgeSSHExec, bridgeSSHSFTP), so a client sees a message and a failed
// command, never a silent hang or an abrupt disconnect.
//
// CLOSES channel itself: every call site is immediately followed by return
// with no further channel use, so this is the one place that needs to do it
// — same deadlock class as sshBridgeExecSession's doc explains (a real
// client's Session.Wait()/Run() blocks for the channel to CLOSE, not merely
// for exit-status to arrive, and the OUTER handleSSHSessionChannel's own
// deferred Close cannot be trusted to run promptly while its request loop is
// still serving window-change concurrently).
func sendChannelError(channel ssh.Channel, msg string) {
	_, _ = fmt.Fprintln(channel.Stderr(), msg)
	sendExitStatus(channel, 1)
	_ = channel.Close()
}

// sendExitStatus reports the exec's exit code on the channel (RFC 4254
// §6.10). Does NOT close channel — sshBridgeExecSession (the one caller that
// uses this directly rather than through sendChannelError) closes it itself
// right after, once Stdin/Stdout copying and Close(sess) are also done.
func sendExitStatus(channel ssh.Channel, code uint32) {
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
}

// clampExitCode folds an out-of-range/unknown exit code (Wait errored: -1)
// into a plain nonzero byte, the same clamp any POSIX shell applies to a
// child's wait status before exposing it as $?.
func clampExitCode(code int) int {
	if code < 0 || code > 255 {
		return 1
	}
	return code
}

// drainExecStderr starts copying sess.Stderr to the channel's own extended-
// data stderr stream IMMEDIATELY — before the caller ever touches Stdout.
//
// HARD CONTRACT (runner.ExecSession's doc): Stdout/Stderr are UNBUFFERED
// io.Pipes fed by ONE demux goroutine; a single undrained stderr byte blocks
// that goroutine, Stdout, AND Wait. Every ExecStream consumer in this file —
// exec, sftp, direct-tcpip/socat — MUST start this before the first Stdout
// read. sftp-server's error path writes stderr before any stdout, which is
// exactly where a missing drain would hang silently.
func drainExecStderr(channel ssh.Channel, sess *runner.ExecSession) {
	if sess.Stderr == nil {
		return
	}
	go func() {
		_, _ = io.Copy(channel.Stderr(), sess.Stderr)
	}()
}

// sshBridgeExecSession bridges channel<->sess for the lifetime of one
// ExecStream session: drains stderr first (see drainExecStderr), pumps
// channel->Stdin and Stdout->channel concurrently, and on Stdout EOF calls
// Wait for the exit code and (when sendExit) reports it via "exit-status".
// ExecSession is documented as a plain, partial-fake-friendly struct (any
// func/stream field may be nil), so every access here is guarded.
//
// CLOSES channel itself before returning (deferred, so every path — including
// a nil Stdout — takes it): sending exit-status is not enough on its own. A
// real ssh client's Session.Wait() blocks for the channel to close, not just
// for exit-status to arrive, and for a "session" channel the OUTER
// handleSSHSessionChannel's own deferred Close cannot be trusted to run this
// promptly — its request loop keeps serving window-change concurrently with
// this bridge, so it is still blocked reading reqs while THIS function would
// otherwise sit idle waiting for the client to hang up first. Neither side
// would ever close, and the exchange deadlocks (caught live by
// TestSSHGateway_ExecExitCode: the fix is this Close, not a client-side
// workaround). direct-tcpip's caller closes independently too (its own defer,
// for its own early-Accept-failure path) — a second Close here is a
// documented-safe no-op (ssh.Channel.Close on an already-closed channel just
// returns an error, never panics).
//
// Also keeps runID's idle clock reset for the life of the stream (TouchRun +
// attachKeepalive, cancelled when this function returns) — a long scp, a
// held -L tunnel, or a slow `ssh run 'make build'` must not be reaped by
// auto_stop mid-flight, same as bridgeSSHShell's own (separate) keepalive
// for the interactive shell path.
//
// Returns the exit code (1 when Wait is absent or errors — clampExitCode's
// same fold) and the total bytes copied Stdout->channel: what ssh.sftp's and
// ssh.forward's "bytes" audit field means — the direction that matters for a
// download/forward is what came OUT of the sandbox.
func (s *Server) sshBridgeExecSession(ctx context.Context, runID uuid.UUID, channel ssh.Channel, sess *runner.ExecSession, sendExit bool) (exitCode int, bytesOut int64) {
	defer channel.Close()
	drainExecStderr(channel, sess)

	keepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	_ = s.cfg.Store.TouchRun(keepCtx, runID)
	go s.attachKeepalive(keepCtx, runID)

	if sess.Stdin != nil {
		go func() {
			_, _ = io.Copy(sess.Stdin, channel)
			_ = sess.Stdin.Close() // half-close only: Stdout/Stderr may still be flowing
		}()
	}

	if sess.Stdout != nil {
		bytesOut, _ = io.Copy(channel, sess.Stdout)
	}

	exitCode = 1
	if sess.Wait != nil {
		if code, err := sess.Wait(); err == nil {
			exitCode = clampExitCode(code)
		}
	}
	if sendExit {
		sendExitStatus(channel, uint32(exitCode)) //nolint:gosec // clampExitCode bounds this to [0,255]
	}
	if sess.Close != nil {
		_ = sess.Close()
	}
	return exitCode, bytesOut
}

// handleSSHSessionChannel owns ONE "session" channel for its whole lifetime:
// it keeps draining requests (pty-req/window-change/env/shell/exec/
// subsystem) until the client closes the channel, dispatching at most ONE of
// shell/exec/subsystem — the first the client sends, a second is refused,
// mirroring a real sshd's one-exec-per-channel rule — to its own bridge
// goroutine while continuing to serve window-change concurrently (a live PTY
// resize must keep working after the shell starts).
func (s *Server) handleSSHSessionChannel(ctx context.Context, runID uuid.UUID, principal string, newCh ssh.NewChannel) {
	channel, reqs, err := newCh.Accept()
	if err != nil {
		return
	}
	// Backstop only: the client-disconnects-first path (reqs closes with no
	// shell/exec/subsystem ever dispatched, or a still-running bridge is
	// interrupted). The COMPLETION path closes channel itself, from inside the
	// bridge goroutine (see sshBridgeExecSession/bridgeSSHShell) — this defer
	// cannot run until the loop below AND the bridge both finish, so it must
	// never be the only thing that closes a finished exec/shell/subsystem.
	defer channel.Close()

	var cols, rows uint16
	started := false
	// "Latest wins": window-change events coalesce onto a 1-slot buffered
	// channel so a burst of resizes never blocks this request loop — only the
	// most recent size matters anyway.
	resizeCh := make(chan sshWindowChangeMsg, 1)
	var env []string
	var bridgeDone chan struct{}

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if started {
				_ = req.Reply(false, nil)
				continue
			}
			var m sshPTYReqMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			cols, rows = uint16(m.Columns), uint16(m.Rows)
			_ = req.Reply(true, nil)

		case "env":
			// Bounded on two axes: !started (no point capturing env after
			// exec/shell/subsystem already dispatched — nothing reads it
			// again) and len(env) < sshMaxEnvVars (an unbounded client could
			// otherwise grow this slice forever pre-dispatch).
			var m sshEnvMsg
			if !started && len(env) < sshMaxEnvVars && ssh.Unmarshal(req.Payload, &m) == nil && sshEnvAllowed(m.Name) {
				env = append(env, m.Name+"="+m.Value)
			}
			_ = req.Reply(true, nil)

		case "window-change":
			var m sshWindowChangeMsg
			if err := ssh.Unmarshal(req.Payload, &m); err == nil {
				select {
				case resizeCh <- m:
				default:
					select {
					case <-resizeCh:
					default:
					}
					resizeCh <- m
				}
			}
			// window-change never carries WantReply per RFC 4254 §6.7, but
			// reply if a client asks anyway rather than assume.
			if req.WantReply {
				_ = req.Reply(true, nil)
			}

		case "shell":
			if started {
				_ = req.Reply(false, nil)
				continue
			}
			started = true
			_ = req.Reply(true, nil)
			bridgeDone = make(chan struct{})
			sshGo(func() {
				defer close(bridgeDone)
				s.bridgeSSHShell(ctx, runID, principal, channel, cols, rows, resizeCh)
			})

		case "exec":
			if started {
				_ = req.Reply(false, nil)
				continue
			}
			var m sshExecReqMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			started = true
			_ = req.Reply(true, nil)
			bridgeDone = make(chan struct{})
			command := m.Command
			sshGo(func() {
				defer close(bridgeDone)
				s.bridgeSSHExec(ctx, runID, principal, channel, command, env)
			})

		case "subsystem":
			if started {
				_ = req.Reply(false, nil)
				continue
			}
			var m sshSubsystemMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil || m.Subsystem != "sftp" {
				_ = req.Reply(false, nil)
				continue
			}
			started = true
			_ = req.Reply(true, nil)
			bridgeDone = make(chan struct{})
			sshGo(func() {
				defer close(bridgeDone)
				s.bridgeSSHSFTP(ctx, runID, principal, channel)
			})

		default:
			// Everything else — agent forwarding (auth-agent-req@openssh.com),
			// X11 (x11-req), signals — is refused BY OMISSION: never acked, so
			// the client never gets what it asked for, and (session-level, see
			// handleSSHConn) the corresponding side-channel type is never
			// accepted either. No protocol reimplementation needed to "reject"
			// what is simply never granted.
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
	if bridgeDone != nil {
		<-bridgeDone
	}
}

// bridgeSSHShell mirrors handleAttachWS's pump (attach.go), over an
// ssh.Channel instead of a WebSocket: the SAME Runner.Attach call, the SAME
// masked live recorder (newSessionRecorder — sessionID prefixed "ssh-" so the
// run's recording tab lists it distinctly from a web-terminal attach under
// the identical CastKey addressing, zero UI work), the SAME TouchRun
// keepalive, and the SAME session.attach/session.detach audit pair (Data
// additionally carries transport:ssh, per the brief).
func (s *Server) bridgeSSHShell(ctx context.Context, runID uuid.UUID, principal string, channel ssh.Channel, cols, rows uint16, resizeCh <-chan sshWindowChangeMsg) {
	// CLOSES channel itself on every return path — see sshBridgeExecSession's
	// doc for why the OUTER handleSSHSessionChannel's own deferred Close
	// cannot be trusted to run promptly here (same deadlock class, same fix).
	defer channel.Close()
	run, msg := s.sshFreshRun(ctx, runID)
	if msg != "" {
		sendChannelError(channel, msg)
		return
	}
	opts := runner.AttachOptions{Cols: cols, Rows: rows}
	sess, err := s.cfg.Runner.Attach(ctx, run.SandboxRef, opts)
	if err != nil {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "session.attach",
			runID.String(), "failure", mustJSON(map[string]any{"transport": "ssh", "error": err.Error()})))
		sendChannelError(channel, "attach failed: "+err.Error())
		return
	}
	defer sess.Close() // tears down ONLY the exec stream, never the sandbox.

	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "session.attach",
		runID.String(), "success", mustJSON(map[string]any{"transport": "ssh", "cols": cols, "rows": rows})))

	// See attach.go's newSessionRecorder doc for the full masking/limitations
	// story (verbatim-only masking, output-direction-only, buffered-in-memory)
	// — reused as-is; the "ssh-" prefix is the only thing distinguishing this
	// call from the web terminal's.
	sessionID := "ssh-" + uuid.New().String()
	castTee, finishRecording := s.newSessionRecorder(run, sessionID, opts)

	pumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	_ = s.cfg.Store.TouchRun(pumpCtx, runID)
	go s.attachKeepalive(pumpCtx, runID)

	closeReason := s.sshShellPump(pumpCtx, channel, sess, runID, castTee, resizeCh)
	cancel()

	// Use BaseCtx (daemon-lifetime), not ctx (the connection's, cancelled the
	// instant it closes — exactly when this runs) — see attach.go's identical
	// FINDING comment on why the request/connection ctx drops this write.
	finishCtx := s.cfg.BaseCtx
	finishRecording(finishCtx, types.ActorHuman, principal)

	s.recordAudit(finishCtx, s.auditEvent(&runID, types.ActorHuman, principal, "session.detach",
		runID.String(), "success", mustJSON(map[string]any{"transport": "ssh", "reason": closeReason})))

	sendExitStatus(channel, 0)
}

// sshShellPump mirrors attach.go's attachPump over an ssh.Channel instead of
// a WebSocket: raw bytes need no framing (no resize control-message hack —
// window-change already arrives out-of-band as its own SSH request, fed in
// via resizeCh by handleSSHSessionChannel). castTee (nil when RecordingStore
// is unset or the run is unrecordable) receives a copy of every chunk of PTY
// output, exactly like the web-terminal attach.
func (s *Server) sshShellPump(ctx context.Context, channel ssh.Channel, sess runner.Session, runID uuid.UUID, castTee io.Writer, resizeCh <-chan sshWindowChangeMsg) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reasonCh := make(chan string, 2)

	// Session -> channel (+ cast tee).
	go func() {
		buf := make([]byte, attachReadBuf)
		for {
			n, rerr := sess.Read(buf)
			if n > 0 {
				if _, werr := channel.Write(buf[:n]); werr != nil {
					reasonCh <- "client write failed"
					cancel()
					return
				}
				if castTee != nil {
					_, _ = castTee.Write(buf[:n])
				}
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					reasonCh <- "shell exited"
				} else {
					reasonCh <- "session read error"
				}
				cancel()
				return
			}
		}
	}()

	// channel -> Session (keystrokes).
	go func() {
		buf := make([]byte, attachReadBuf)
		for {
			n, rerr := channel.Read(buf)
			if n > 0 {
				// Any client traffic counts as activity: keep the session alive.
				_ = s.cfg.Store.TouchRun(ctx, runID)
				if _, werr := sess.Write(buf[:n]); werr != nil {
					reasonCh <- "session write failed"
					cancel()
					return
				}
			}
			if rerr != nil {
				reasonCh <- "client closed"
				cancel()
				return
			}
		}
	}()

	// Resize.
	go func() {
		for {
			select {
			case m, ok := <-resizeCh:
				if !ok {
					return
				}
				_ = sess.Resize(ctx, uint16(m.Columns), uint16(m.Rows))
			case <-ctx.Done():
				return
			}
		}
	}()

	<-ctx.Done()
	select {
	case reason := <-reasonCh:
		return reason
	default:
		return "context cancelled"
	}
}

// bridgeSSHExec runs command through /bin/sh -c inside the sandbox via
// ExecStream — NO protocol reimplementation: word-splitting/quoting is the
// SANDBOX's own shell's job, exactly like a real sshd's `${SHELL} -c
// command`. TTY=false (separate stdout/stderr, per the brief); env is the
// caller's TERM/LANG/LC_*-allowlisted set collected from "env" requests.
// Never recorded — distinct from bridgeSSHShell's Attach call, this is
// runner.ExecStream's "fresh, streamable exec... no PTY tmux session, no
// shell wrapping" primitive, with no masking pipeline attached (which is
// exactly why sftp's and direct-tcpip's binary streams are safe to run
// through the SAME bridge as this one, below).
func (s *Server) bridgeSSHExec(ctx context.Context, runID uuid.UUID, principal string, channel ssh.Channel, command string, env []string) {
	run, msg := s.sshFreshRun(ctx, runID)
	if msg != "" {
		sendChannelError(channel, msg)
		return
	}
	sess, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{
		Argv: []string{"/bin/sh", "-c", command},
		Env:  env,
	})
	if err != nil {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.exec",
			runID.String(), "failure", mustJSON(map[string]any{"argv": command, "error": err.Error()})))
		sendChannelError(channel, sshExecStreamErrorMessage(err))
		return
	}
	exit, _ := s.sshBridgeExecSession(ctx, runID, channel, sess, true)
	// FINDING (medium, fixed — caught by the live SSH e2e's -L forward step,
	// scripts/run-e2e-ssh.sh): every trailing write in this file that follows
	// sshBridgeExecSession (here, bridgeSSHSFTP, handleSSHDirectTCPIP) used to
	// run on ctx — handleSSHConn's connCtx, cancelled the INSTANT the whole
	// SSH connection tears down. A client that kills its session mid-flight
	// races that cancellation against this exact line; lose the race and the
	// write is attempted on an already-cancelled context. The primary store
	// write then fails fast (context.Canceled, no query even sent) and
	// recordAudit swallows that error — the row surfaces only after the audit
	// spool's own drain cycle (auditSpoolDrainInterval later — invisible to a
	// poll right after the kill) if a spool is configured at all, or is
	// dropped outright if it isn't. Same fix as bridgeSSHShell's
	// session.detach write above / attach.go's identical FINDING: use
	// s.cfg.BaseCtx (daemon-lifetime) for a write that must outlive the
	// connection it is reporting the end of.
	s.recordAudit(s.cfg.BaseCtx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.exec",
		runID.String(), "success", mustJSON(map[string]any{"argv": command, "exit": exit})))
}

// bridgeSSHSFTP runs the sandbox's own sftp-server as the subsystem's backing
// process — no SFTP protocol reimplementation. A BYOI image without the
// binary surfaces as a clean channel error naming the image contract (never
// a hang): ErrExecStreamUnsupported maps to the shared honest message every
// primitive here uses, and any other ExecStream launch failure names the
// missing path directly.
func (s *Server) bridgeSSHSFTP(ctx context.Context, runID uuid.UUID, principal string, channel ssh.Channel) {
	run, msg := s.sshFreshRun(ctx, runID)
	if msg != "" {
		sendChannelError(channel, msg)
		return
	}
	sess, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{Argv: []string{sftpServerPath, "-e"}})
	if err != nil {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sftp",
			runID.String(), "failure", mustJSON(map[string]any{"error": err.Error()})))
		reason := sshExecStreamErrorMessage(err)
		if !errors.Is(err, runner.ErrExecStreamUnsupported) {
			reason = "sftp unavailable: the sandbox image has no " + sftpServerPath + " (" + err.Error() + ")"
		}
		sendChannelError(channel, reason)
		return
	}
	_, bytesOut := s.sshBridgeExecSession(ctx, runID, channel, sess, true)
	// BaseCtx, not ctx — see bridgeSSHExec's identical trailing-write FINDING
	// comment above (same shape, same connection-teardown race, same fix).
	s.recordAudit(s.cfg.BaseCtx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sftp",
		runID.String(), "success", mustJSON(map[string]any{"bytes": bytesOut})))
}

// handleSSHDirectTCPIP implements `-L` local forwarding: ExecStream of `socat
// - TCP:127.0.0.1:<port>` inside the sandbox's OWN network namespace (the
// sandbox cannot create egress — L0 structural confinement, invariant 3, is
// unaffected by this), so the only reachable destination is EVER the
// sandbox's own loopback regardless of what host the client asked to
// forward to. dst is validated against that BEFORE ExecStream is even
// attempted — a non-loopback ask is refused (fail closed) with a clear
// reason, never silently rewritten.
func (s *Server) handleSSHDirectTCPIP(ctx context.Context, runID uuid.UUID, principal string, newCh ssh.NewChannel) {
	var m sshDirectTCPIPMsg
	if err := ssh.Unmarshal(newCh.ExtraData(), &m); err != nil {
		_ = newCh.Reject(ssh.Prohibited, "malformed forwarding request")
		return
	}
	if !sshIsSandboxLoopback(m.DestHost) {
		_ = newCh.Reject(ssh.Prohibited, "forwarding destination must be the sandbox's own loopback (127.0.0.1/::1/localhost) — the sandbox has no other egress")
		return
	}
	if m.DestPort == 0 || m.DestPort > 65535 {
		_ = newCh.Reject(ssh.Prohibited, "invalid destination port")
		return
	}

	run, msg := s.sshFreshRun(ctx, runID)
	if msg != "" {
		_ = newCh.Reject(ssh.ConnectionFailed, msg)
		return
	}

	sess, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{
		Argv: []string{"socat", "-", fmt.Sprintf("TCP:127.0.0.1:%d", m.DestPort)},
	})
	if err != nil {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.forward",
			fmt.Sprintf("127.0.0.1:%d", m.DestPort), "failure",
			mustJSON(map[string]any{"port": m.DestPort, "error": err.Error()})))
		_ = newCh.Reject(ssh.ConnectionFailed, sshExecStreamErrorMessage(err))
		return
	}

	channel, reqs, err := newCh.Accept()
	if err != nil {
		if sess.Close != nil {
			_ = sess.Close()
		}
		return
	}
	defer channel.Close()
	go ssh.DiscardRequests(reqs)

	_, bytesOut := s.sshBridgeExecSession(ctx, runID, channel, sess, false)
	// BaseCtx, not ctx — see bridgeSSHExec's identical trailing-write FINDING
	// comment (same shape, same connection-teardown race, same fix). THIS is
	// the exact call the live e2e's -L forward step caught losing its
	// ssh.forward row to a killed session (scripts/run-e2e-ssh.sh, step 5's
	// comment); TestSSHGateway_ForwardAuditSurvivesKill pins it.
	s.recordAudit(s.cfg.BaseCtx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.forward",
		fmt.Sprintf("127.0.0.1:%d", m.DestPort), "success",
		mustJSON(map[string]any{"port": m.DestPort, "bytes": bytesOut})))
}
