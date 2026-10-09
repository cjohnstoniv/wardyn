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
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sshDisplaceGrace is how long a displaced SSH client gets to accept the
// take-over reason on its stderr before the pump is cancelled anyway (see the
// displace closure in bridgeSSHShell). A draining client takes microseconds; a
// client that is not draining is precisely the one that must not hold the
// evicted session open. Well under the take-over's own visible budget, so the
// operator who clicked never waits on it.
const sshDisplaceGrace = time.Second

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
// same fold) and the bytes copied each way: bytesOut is Stdout->channel, what
// ssh.sftp.transfer's and ssh.forward's "bytes" audit field means — the
// direction that matters for a download/forward is what came OUT of the
// sandbox — and bytesIn is channel->Stdin, what the sandbox received.
func (s *Server) sshBridgeExecSession(ctx context.Context, runID uuid.UUID, principal string, channel ssh.Channel, sess *runner.ExecSession, sendExit bool) (exitCode int, bytesIn, bytesOut int64) {
	defer channel.Close()
	drainExecStderr(channel, sess)

	// The io.Copy off sess.Stdout below reads a docker exec pipe that no ctx
	// can reach — a disconnected client (or a closed channel) would otherwise
	// park this goroutine until the in-sandbox command exits, holding one of
	// the run's four channel slots AND keeping the keepalive below touching
	// the run so the idle reaper never fires. Close the session when ctx dies;
	// Close is idempotent on both drivers.
	stopOnCtx := context.AfterFunc(ctx, func() {
		if sess.Close != nil {
			_ = sess.Close()
		}
	})
	defer stopOnCtx()

	keepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	_ = s.cfg.Store.TouchRun(keepCtx, runID)
	go s.attachKeepalive(keepCtx, runID)

	var in atomic.Int64
	if sess.Stdin != nil {
		go func() {
			// Bytes a person sends are presence (run_pause.go).
			src := presenceReader{r: channel, mark: func() { _ = s.markPresent(ctx, runID, types.ActorHuman, principal, "presence") }}
			// Counted as written to the sandbox, off this goroutine: it may still
			// be mid-copy when Stdout ends, so the total is read atomically.
			_, _ = io.Copy(countingWriter{w: sess.Stdin, n: &in}, src)
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
	return exitCode, in.Load(), bytesOut
}

// countingWriter adds each successful write's size to n.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	k, err := c.w.Write(p)
	c.n.Add(int64(k))
	return k, err
}

// sshSessionGate is one "session" channel's dispatch state: whether a
// shell/exec/subsystem already started, and the budget slot it holds.
type sshSessionGate struct {
	s          *Server
	ctx        context.Context
	runID      uuid.UUID
	principal  string
	slot       *sshChannelSlot
	channel    ssh.Channel
	started    bool
	capAudited bool
}

// mayDispatch refuses (and replies false to) a second dispatch, and — when
// shared is set — one a sync-only channel (admitted past the shared cap, see
// sshOpenSlot) may not make: that one is audited once, answered with the cap
// text and closed, which frees the sync slot it was holding.
func (g *sshSessionGate) mayDispatch(req *ssh.Request, shared bool) bool {
	if g.started {
		_ = req.Reply(false, nil)
		return false
	}
	if shared && g.slot.sync {
		if !g.capAudited {
			g.capAudited = true
			g.s.sshAuditChannelRejected(g.ctx, g.runID, g.principal, "session", sshCapReasonChannels, g.s.cfg.SSHMaxSessionsPerRun)
		}
		_ = req.Reply(false, nil)
		sendChannelError(g.channel, fmt.Sprintf("too many concurrent SSH channels for this run (max %d)", g.s.cfg.SSHMaxSessionsPerRun))
		return false
	}
	return true
}

// subsystemBridge validates a subsystem request and returns the bridge to run,
// or nil after replying false. wardyn-sync moves the channel onto a sync slot.
func (g *sshSessionGate) subsystemBridge(req *ssh.Request, channel ssh.Channel, syncEnv map[string]string) func() {
	s, ctx, runID, principal := g.s, g.ctx, g.runID, g.principal
	var m sshSubsystemMsg
	if err := ssh.Unmarshal(req.Payload, &m); err != nil {
		_ = req.Reply(false, nil)
		return nil
	}
	switch m.Subsystem {
	case "sftp":
		if g.mayDispatch(req, true) {
			return func() { s.bridgeSSHSFTP(ctx, runID, principal, channel) }
		}
	case sshSyncSubsystem:
		if !g.mayDispatch(req, false) {
			return nil
		}
		if !g.slot.toSync() {
			if !g.capAudited {
				g.capAudited = true
				s.sshAuditChannelRejected(ctx, runID, principal, "session", sshCapReasonSync, maxSSHSyncSessionsPerRun)
			}
			_ = req.Reply(false, nil)
			return nil
		}
		return func() { s.bridgeSSHSync(ctx, runID, principal, channel, syncEnv) }
	default:
		_ = req.Reply(false, nil)
	}
	return nil
}

// handleSSHSessionChannel owns ONE "session" channel for its whole lifetime:
// it keeps draining requests (pty-req/window-change/env/shell/exec/
// subsystem) until the client closes the channel, dispatching at most ONE of
// shell/exec/subsystem — the first the client sends, a second is refused,
// mirroring a real sshd's one-exec-per-channel rule — to its own bridge
// goroutine while continuing to serve window-change concurrently (a live PTY
// resize must keep working after the shell starts).
func (s *Server) handleSSHSessionChannel(ctx context.Context, runID uuid.UUID, principal string, newCh ssh.NewChannel, slot *sshChannelSlot) {
	channel, reqs, err := newCh.Accept()
	if err != nil {
		return
	}
	// Channel-scoped ctx for the bridges: cancelled when THIS channel's request
	// stream ends (client closed the channel) as well as when the parent
	// connection ctx dies — the bridge's AfterFunc closes its exec session on
	// either, so a parked io.Copy can never outlive its channel.
	chCtx, chCancel := context.WithCancel(ctx)
	defer chCancel()
	// Backstop only: the client-disconnects-first path (reqs closes with no
	// shell/exec/subsystem ever dispatched, or a still-running bridge is
	// interrupted). The COMPLETION path closes channel itself, from inside the
	// bridge goroutine (see sshBridgeExecSession/bridgeSSHShell) — this defer
	// cannot run until the loop below AND the bridge both finish, so it must
	// never be the only thing that closes a finished exec/shell/subsystem.
	defer channel.Close()

	var cols, rows uint16
	// "Latest wins": window-change events coalesce onto a 1-slot buffered
	// channel so a burst of resizes never blocks this request loop — only the
	// most recent size matters anyway.
	resizeCh := make(chan sshWindowChangeMsg, 1)
	var env []string
	// syncEnv holds the wardyn-sync-only env names. Kept apart from env so they
	// can never reach an exec or shell: only a wardyn-sync dispatch reads them.
	syncEnv := map[string]string{}
	var bridgeDone chan struct{}
	gate := &sshSessionGate{s: s, ctx: chCtx, runID: runID, principal: principal, slot: slot, channel: channel}

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if gate.started {
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
			if !gate.started && ssh.Unmarshal(req.Payload, &m) == nil {
				switch {
				case sshSyncEnvName(m.Name):
					syncEnv[m.Name] = m.Value
				case len(env) < sshMaxEnvVars && sshEnvAllowed(m.Name):
					env = append(env, m.Name+"="+m.Value)
				}
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
			if !gate.mayDispatch(req, true) {
				continue
			}
			gate.started = true
			_ = req.Reply(true, nil)
			bridgeDone = make(chan struct{})
			sshGo(func() {
				defer close(bridgeDone)
				s.bridgeSSHShell(chCtx, runID, principal, channel, cols, rows, resizeCh)
			})

		case "exec":
			if !gate.mayDispatch(req, true) {
				continue
			}
			var m sshExecReqMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			gate.started = true
			_ = req.Reply(true, nil)
			bridgeDone = make(chan struct{})
			command := m.Command
			sshGo(func() {
				defer close(bridgeDone)
				s.bridgeSSHExec(chCtx, runID, principal, channel, command, env)
			})

		case "subsystem":
			bridge := gate.subsystemBridge(req, channel, syncEnv)
			if bridge == nil {
				continue
			}
			gate.started = true
			_ = req.Reply(true, nil)
			bridgeDone = make(chan struct{})
			sshGo(func() {
				defer close(bridgeDone)
				bridge()
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
	// The request loop is done — the CLIENT closed this channel (or the
	// connection died). A bridge parked on a quiet exec would wait for the
	// in-sandbox command forever; cancel the channel-scoped ctx so its
	// AfterFunc closes the session and the bridge returns. A normally-finished
	// bridge already closed the channel itself, making this a no-op.
	chCancel()
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
	run, msg := s.sshFreshRun(ctx, runID, principal)
	if msg != "" {
		sendChannelError(channel, msg)
		return
	}
	// Door 4 of five (mask_manifest.go): the shell's recorded tail is masked
	// against the run's corpus, which this server must be able to prove whole.
	if !s.maskCovered(ctx, runID) {
		s.auditUncovered(ctx, runID, types.ActorHuman, principal, "ssh.shell")
		sendChannelError(channel, "wardyn: this server cannot prove this run's secrets are masked right now; the shell is refused")
		return
	}

	// See attach.go's newSessionRecorder doc for the full masking/limitations
	// story (verbatim-only masking, output-direction-only, buffered-in-memory)
	// — reused as-is; the "ssh-" prefix is the only thing distinguishing this
	// call from the web terminal's.
	sessionID := "ssh-" + uuid.New().String()
	// The recorder gets the CLIENT's real geometry for its cast header.
	castTee, finishRecording := s.newSessionRecorder(run, sessionID, runner.AttachOptions{Cols: cols, Rows: rows})

	// pumpCtx ends the whole bridge: the exec being opened, the pump, the
	// keepalive and the mask-fence watcher all hang off it.
	pumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Use BaseCtx (daemon-lifetime), not ctx (the connection's, cancelled the
	// instant it closes — exactly when the outcome and the detach are recorded)
	// — see attach.go's identical FINDING comment on why the request/connection
	// ctx drops these writes.
	finishCtx := s.cfg.BaseCtx
	if finishCtx == nil {
		finishCtx = context.Background()
	}

	// THE SAME holder registry the web terminal uses (attach_holder.go), with
	// source "ssh". Not optional: a CLI holder over the gateway is attached to
	// the identical shared tmux session, so leaving it unregistered would have
	// the browser confidently report "nobody is attached" while somebody is
	// typing — and would let the run page silently start competing for the PTY,
	// which is the whole failure this registry exists to end.
	//
	// Registration comes BEFORE the exec exists (attach_exec.go), the same order
	// as the web lane: the role decides the attach options. A writer opens at the
	// pty-req size; an observer opens with tmux's ignore-size flag at the
	// writer's size, so it never resizes the writer's window. The old advisory
	// pre-check ("is someone holding?") raced in the arrive direction — two
	// clients could both read nil and the loser's exec still clamped the
	// winner's tmux window.
	var holder *attachHolder
	holder = &attachHolder{
		principal: principal,
		actorType: types.ActorHuman,
		since:     s.cfg.Now().UTC(),
		source:    attachSourceSSH,
		onInput:   func() { _ = s.markPresent(ctx, runID, types.ActorHuman, principal, "presence") },
		cols:      cols,
		rows:      rows,
		// Promotion: this channel just became the writer without reconnecting.
		// Its exec is still the observer's, so a writer exec is opened at its own
		// size first (the RESIZE is the act), and the line is a courtesy after
		// it: channel.Stderr() is the unbounded write that stranded the displace
		// path (no context, no deadline, blocked on the peer's window), and a
		// courtesy that can park forever must never sit in front of the one call
		// that matters. A writer-resize notice (readOnly=true) has nothing to
		// say on this lane: the observer's PTY already follows the writer.
		notify: func(readOnly bool, h *attachHolder) {
			if readOnly {
				return
			}
			if err := s.establishExec(pumpCtx, runID, holder, run.SandboxRef); err != nil {
				if errors.Is(err, errAttachCancelled) {
					return // evicted or gone meanwhile: its channel is closing by other means
				}
				slog.WarnContext(pumpCtx, "wardynd: promoted ssh attach could not open a writer exec", "run_id", runID, "err", err)
				s.recordAudit(finishCtx, s.auditEvent(&runID, types.ActorHuman, principal, "session.attach",
					runID.String(), "failure", mustJSON(map[string]any{"transport": "ssh", "state": "attaching", "error": err.Error(), "promotion": true})))
				_, _ = fmt.Fprintln(channel.Stderr(), "wardyn: could not take over this terminal: "+err.Error())
				cancel()
				return
			}
			_, _ = fmt.Fprintln(channel.Stderr(), "wardyn: you now hold this terminal")
		},
		displace: func(reason string) {
			// The SSH lane's equivalent of the WebSocket close frame: a line on
			// the channel's stderr, which ssh(1) prints to the operator's own
			// terminal, and the pump cancel that ends the session. On a
			// goroutine so a client that has stopped reading its window cannot
			// wedge the take-over HTTP request behind a blocked write.
			//
			// The cancel is the act; the line is a courtesy, so the cancel is on
			// a TIMER the write cannot outlive. x/crypto's WriteExtended blocks
			// on the channel's remote window with no context and no deadline, so
			// a displaced client that stopped reading (a suspended ssh(1)) used
			// to park this goroutine forever: cancel never ran, and the evicted
			// pump kept its exec (a tmux client on the SHARED session), its
			// TouchRun keepalive (the idle reaper never fires) and one of the
			// run's four channel slots until the TCP connection died. Write
			// authority was already revoked, so what leaked was the session, not
			// a second writer — which is exactly why the courtesy may wait a
			// little and the teardown may not.
			go func() {
				late := time.AfterFunc(sshDisplaceGrace, cancel)
				_, _ = fmt.Fprintln(channel.Stderr(), "wardyn: "+reason)
				late.Stop()
				cancel()
			}()
		},
	}
	readOnly, releaseHolder := s.registerAttachHolder(runID, holder)
	// Deferred for the same reason as the web lane's: a panicking pump must
	// never strand a phantom holder (see registerAttachHolder). releaseAttach
	// also delivers the promotion this release may cause, on its own goroutine.
	defer releaseAttach(releaseHolder)
	// Tears down ONLY the exec stream, never the sandbox.
	defer func() { _ = holder.mux.Close() }()

	// The first of two session.attach rows (see handleAttachWS): registered, exec
	// not yet open.
	s.recordAudit(finishCtx, s.auditEvent(&runID, types.ActorHuman, principal, "session.attach",
		runID.String(), "success", mustJSON(map[string]any{
			"transport": "ssh", "cols": cols, "rows": rows, "state": "attaching", "read_only": readOnly,
		})))
	if readOnly {
		// The holder object STAYS (holder.writable is the observer marker now);
		// sshShellPump drops this client's keystrokes while canWrite is false, and
		// the same object flips on promotion.
		msg := "wardyn: read-only — another client holds this terminal"
		if cur := s.attachHolderFor(runID); cur != nil {
			msg = "wardyn: read-only — " + cur.principal + " (" + cur.source + ") holds this terminal; take it over from the run page"
		}
		_, _ = fmt.Fprintln(channel.Stderr(), msg)
	}

	// Started before the exec is opened, so a fence ends an attach in flight too.
	maskFenced := s.endSSHOnMaskFence(pumpCtx, runID, channel, cancel)

	var closeReason string
	attachErr := s.establishExec(pumpCtx, runID, holder, run.SandboxRef)
	if attachErr != nil {
		s.recordAudit(finishCtx, s.auditEvent(&runID, types.ActorHuman, principal, "session.attach",
			runID.String(), "failure", mustJSON(map[string]any{"transport": "ssh", "state": "attaching", "error": attachErr.Error()})))
		if !errors.Is(attachErr, errAttachCancelled) {
			sendChannelError(channel, "attach failed: "+attachErr.Error())
		}
		closeReason = "attach failed"
	} else {
		s.recordAudit(finishCtx, s.auditEvent(&runID, types.ActorHuman, principal, "session.attach",
			runID.String(), "success", mustJSON(map[string]any{
				"transport": "ssh", "state": "ready", "read_only": !holder.writable.Load(),
			})))
		_ = s.cfg.Store.TouchRun(pumpCtx, runID)
		go s.attachKeepalive(pumpCtx, runID)
		closeReason = s.sshShellPump(pumpCtx, runID, channel, castTee, resizeCh, holder)
	}
	cancel()
	if maskFenced() {
		closeReason = maskFencedReason
	}

	finishRecording(finishCtx, types.ActorHuman, principal)

	// read_only is the LIVE flag (see attach.go's twin): a channel that
	// arrived as an observer and was promoted mid-session detaches as a writer.
	s.recordAudit(finishCtx, s.auditEvent(&runID, types.ActorHuman, principal, "session.detach",
		runID.String(), "success", mustJSON(map[string]any{"transport": "ssh", "reason": closeReason, "read_only": !holder.writable.Load()})))

	if attachErr == nil {
		sendExitStatus(channel, 0)
	}
}

// sshShellPump mirrors attach.go's attachPump over an ssh.Channel instead of
// a WebSocket: raw bytes need no framing (no resize control-message hack —
// window-change already arrives out-of-band as its own SSH request, fed in
// via resizeCh by handleSSHSessionChannel). castTee (nil when RecordingStore
// is unset or the run is unrecordable) receives a copy of every chunk of PTY
// output, exactly like the web-terminal attach.
//
// holder mirrors attachPump's: this channel's registry entry, whose canWrite
// says whether it may drive the PTY right now. A read-only observer's
// keystrokes are dropped here, server-side, and its window-change is kept as
// its own geometry (for its promotion) without ever reaching the shared tmux
// window: tmux sizes a shared window to its latest client, which is why an
// observer attaches with ignore-size and follows the writer's size instead. The
// channel is still READ from while read-only, because that read is how this
// pump learns the client hung up.
func (s *Server) sshShellPump(ctx context.Context, runID uuid.UUID, channel ssh.Channel, castTee io.Writer, resizeCh <-chan sshWindowChangeMsg, holder *attachHolder) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &sshPumpState{
		s: s, ctx: ctx, cancel: cancel, runID: runID, channel: channel, castTee: castTee, holder: holder,
		// The holder's mux follows its current exec, so a promotion's re-attach
		// replaces the exec under these goroutines.
		sess:     &holder.mux,
		reasonC:  make(chan string, 2),
		resizeCh: resizeCh,
	}
	go p.output()
	go p.input()
	go p.resize()

	<-ctx.Done()
	select {
	case reason := <-p.reasonC:
		return reason
	default:
		return "context cancelled"
	}
}

// sshPumpState is one SSH shell's relay: the goroutines sshShellPump starts,
// and what they share.
type sshPumpState struct {
	s        *Server
	ctx      context.Context
	cancel   context.CancelFunc
	runID    uuid.UUID
	channel  ssh.Channel
	castTee  io.Writer
	holder   *attachHolder
	sess     runner.Session
	reasonC  chan string
	resizeCh <-chan sshWindowChangeMsg
}

func (p *sshPumpState) end(reason string) {
	select {
	case p.reasonC <- reason:
	default:
	}
	p.cancel()
}

// output is Session -> channel (+ cast tee).
func (p *sshPumpState) output() {
	buf := make([]byte, attachReadBuf)
	for {
		n, rerr := p.sess.Read(buf)
		if n > 0 {
			if _, werr := p.channel.Write(buf[:n]); werr != nil {
				p.end("client write failed")
				return
			}
			if p.castTee != nil {
				_, _ = p.castTee.Write(buf[:n])
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				p.end("shell exited")
			} else {
				p.end("session read error")
			}
			return
		}
	}
}

// input is channel -> Session (keystrokes).
func (p *sshPumpState) input() {
	buf := make([]byte, attachReadBuf)
	for {
		n, rerr := p.channel.Read(buf)
		if n > 0 {
			// Held while a promotion replaces the exec: nothing is written until
			// it matches the role, and writeGated re-tests authority.
			if !p.holder.ready.wait(p.ctx) {
				return
			}
			// Same eviction-aware write path as the web pump — one gate for both
			// transports (attach_holder.go writeGated).
			if werr := p.holder.writeGated(p.sess, buf[:n]); werr != nil {
				p.end("session write failed")
				return
			}
		}
		if rerr != nil {
			p.end("client closed")
			return
		}
	}
}

// resize applies window-changes.
func (p *sshPumpState) resize() {
	for {
		select {
		case m, ok := <-p.resizeCh:
			if !ok {
				return
			}
			if !p.applyResize(uint16(m.Columns), uint16(m.Rows)) {
				return
			}
		case <-p.ctx.Done():
			return
		}
	}
}

// applyResize handles one window-change and reports false when the pump ended.
func (p *sshPumpState) applyResize(cols, rows uint16) bool {
	if cols == 0 || rows == 0 {
		return true
	}
	// Keep the registry's geometry LIVE — the pty-req value is stale the moment
	// the operator resizes their terminal. For an observer it is its OWN
	// geometry, which its promotion opens a writer exec at.
	p.holder.setSize(cols, rows)
	// canWrite, not `holder == nil`: an observer's size never reaches the shared
	// tmux window, and neither does a holder whose authority a take-over already
	// revoked. Gating on nil alone let an EVICTED ssh client keep resizing the
	// window under the new holder until its channel died — the web twin
	// (attach_pump.go's reader) gates on canWrite too.
	if !p.holder.canWrite() {
		return true
	}
	if !p.holder.ready.wait(p.ctx) {
		return false
	}
	if !p.holder.canWrite() {
		return true
	}
	if err := p.sess.Resize(p.ctx, cols, rows); err != nil {
		slog.WarnContext(p.ctx, "wardynd: ssh attach resize failed", "run_id", p.runID, "err", err)
		return true
	}
	p.s.fanoutWriterResize(p.ctx, p.runID, p.holder, cols, rows)
	return true
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
	run, msg := s.sshFreshRun(ctx, runID, principal)
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
	exit, _, _ := s.sshBridgeExecSession(ctx, runID, principal, channel, sess, true)
	// Every trailing write in this file that follows
	// sshBridgeExecSession (here, bridgeSSHSFTP, handleSSHDirectTCPIP) must
	// run on s.cfg.BaseCtx, never ctx — handleSSHConn's connCtx, cancelled the
	// INSTANT the whole SSH connection tears down. A client that kills its
	// session mid-flight races that cancellation against this exact line; on
	// ctx, losing that race attempts the write on an already-cancelled
	// context. The primary store
	// write then fails fast (context.Canceled, no query even sent) and
	// recordAudit swallows that error — the row surfaces only after the audit
	// spool's own drain cycle (auditSpoolDrainInterval later — invisible to a
	// poll right after the kill) if a spool is configured at all, or is
	// dropped outright if it isn't. Same rule as bridgeSSHShell's
	// session.detach write above / attach.go's identical seam: use
	// s.cfg.BaseCtx (daemon-lifetime) for a write that must outlive the
	// connection it is reporting the end of.
	s.recordAudit(s.cfg.BaseCtx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.exec",
		runID.String(), "success", mustJSON(map[string]any{"argv": command, "exit": exit})))
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

	run, msg := s.sshFreshRun(ctx, runID, principal)
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

	_, _, bytesOut := s.sshBridgeExecSession(ctx, runID, principal, channel, sess, false)
	// BaseCtx, not ctx — see bridgeSSHExec's identical trailing-write FINDING
	// comment (same shape, same connection-teardown race, same fix). THIS is
	// the exact call the live e2e's -L forward step caught losing its
	// ssh.forward row to a killed session (scripts/run-e2e-ssh.sh, step 5's
	// comment); TestSSHGateway_ForwardAuditSurvivesKill pins it.
	s.recordAudit(s.cfg.BaseCtx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.forward",
		fmt.Sprintf("127.0.0.1:%d", m.DestPort), "success",
		mustJSON(map[string]any{"port": m.DestPort, "bytes": bytesOut})))
}
