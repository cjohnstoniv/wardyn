// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Port forwarding over the organisation: GET /api/v1/runs/{id}/attach/forward?port=<n>&ticket=<t>.
//
// The route reaches no further than an SSH -L channel does: one TCP connection to a port on the sandbox's own
// loopback, through the same in-sandbox relay (sshRelayArgv) and the same bridge (sshBridgeExecSession), under the
// same per-run channel cap, WARDYN_SSH_CHANNELS, data_out rule and deny_interactive door. It is for a deployment
// whose SSH gateway is off, and it is off itself unless the operator sets WARDYN_ATTACH_FORWARD. Every forward
// needs an attach ticket, an admin's included, so the credential that opened it is checked again every
// attachAuthorityEvery for as long as it is open.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

const (
	attachForwardOffMsg        = "Port forwarding over the organisation is off on this deployment: its operator has not turned on WARDYN_ATTACH_FORWARD."
	attachForwardChannelOffMsg = "Port forwarding is off on this deployment: its operator left forward out of WARDYN_SSH_CHANNELS."
	attachForwardNoTicketMsg   = "Port forwarding needs an attach ticket: mint one with POST /runs/{id}/attach/ticket and pass it as ?ticket=."
	attachForwardTicketMsg     = "This attach ticket does not open a port forward."
	attachForwardPortMsg       = "The port must be a whole number from 1 to 65535."
	attachForwardDataOutMsg    = "This run's data_out rule (%s) keeps content in, so port forwarding is off; the terminal still works."
	attachForwardScreenMsg     = "Forwarding to a macOS VM's Screen Sharing ports (5900-5909) is not allowed: open the run's window instead."
	attachForwardBusyMsg       = "This run already has %d SSH channels, commands or forwards open, the most it may; try again when one ends."
	attachForwardFailedMsg     = "The runner could not start the forward."
)

// The words an open forward's row carries as its error when its re-check or its ping ends it.
const (
	attachForwardEndSession    = attachAuthEndedReason
	attachForwardEndRun        = "The run ended."
	attachForwardEndDataOut    = "The run's data_out rule now keeps content in."
	attachForwardEndPing       = "Ping timeout."
	attachForwardEndReadFailed = "The re-check could not read the run or its data_out rule."
)

// forwardLiveRunWords are this door's refusals for a run that cannot take a forward now.
var forwardLiveRunWords = liveRunWords{
	noRunner:   "This server runs no sandboxes, so the run cannot take a forward.",
	notFound:   "The run was not found.",
	notRunning: "The run is not running (state %s), so it cannot take a forward.",
	ended:      "The run has ended, so it cannot take a forward.",
}

var (
	errAttachForwardPing       = errors.New("attach forward: the peer did not answer a ping")
	errAttachForwardRunEnded   = errors.New("attach forward: the run stopped taking a forward")
	errAttachForwardDataOut    = errors.New("attach forward: the run's data_out rule now keeps content in")
	errAttachForwardReadFailed = errors.New("attach forward: the re-check could not read the run or its data_out rule")
)

// attachForwardOn reports whether this deployment serves port forwarding over the organisation. Every read of the
// switch goes through here, so its backing (a deployment variable today) can change in one place.
func (s *Server) attachForwardOn(_ context.Context) bool { return envEnabled(envAttachForward) }

// handleAttachForward is GET /api/v1/runs/{id}/attach/forward: it relays one TCP connection, as binary WebSocket
// frames, to the port on the sandbox's own loopback, by an attach ticket only.
func (s *Server) handleAttachForward(w http.ResponseWriter, r *http.Request) {
	if s.refuseLocalModeAttach(w, r) {
		return
	}
	// The origin is decided here, as handleAttachWS decides it, and websocket.Accept below is told so.
	if s.attachOriginRefused(r) {
		s.auditAuthFailedAs(r, csrfActor, csrfAuditReason)
		writeErrorReason(w, http.StatusForbidden, csrfAuditReason, csrfRefusedBody)
		return
	}
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	// Ticket only: a token or cookie presented directly on a socket is checked at the handshake alone, so the
	// forward would outlive a revoked credential. Only an admin reaches here without a ticket (requireOperator).
	ta, ok := ticketActorFromContext(r.Context())
	if !ok {
		writeErrorReason(w, http.StatusBadRequest, reasonRunForwardTicketRequired, attachForwardNoTicketMsg)
		return
	}
	run, ok := s.getRunOr404(w, r, id)
	if !ok {
		return
	}
	// A helper's, a portal's and an issuer portal's ticket do not open a forward; refuseAttachEntry refuses an
	// app's and a caller's.
	if ta.support != uuid.Nil || ta.via != nil || !ta.admitBy.IsZero() {
		s.auditAttachDenied(r, id, ta.principal, attachForwardTicketMsg)
		writeErrorReason(w, http.StatusForbidden, reasonAttachTicketNotYourRun, attachForwardTicketMsg)
		return
	}
	// The owner rule and deny_interactive come before the switch, so a caller who may not enter learns nothing
	// from "off".
	if s.refuseAttachEntry(w, r, run) || s.refuseBackgroundOnly(w, r, run, "port forwarding") {
		return
	}
	ctx := r.Context()
	at, actor := actorFromRequest(r)
	fail := func(data map[string]any) {
		s.recordAudit(ctx, s.attachForwardEvent(run.ID, at, actor, "failure", data))
	}
	if !s.attachForwardOn(ctx) {
		fail(map[string]any{"reason": reasonRunForwardOff})
		writeErrorReason(w, http.StatusConflict, reasonRunForwardOff, attachForwardOffMsg)
		return
	}
	if !s.sshChannelOn(sshKindForward) {
		s.sshAuditChannelOff(ctx, run.ID, at, actor, string(sshKindForward))
		writeErrorReason(w, http.StatusConflict, reasonRunForwardOff, attachForwardChannelOffMsg)
		return
	}
	port, ok := attachForwardPort(r.URL.Query().Get("port"))
	if !ok {
		writeErrorReason(w, http.StatusBadRequest, reasonRunForwardInvalid, attachForwardPortMsg)
		return
	}
	if s.refuseForwardDataOut(w, r, run.ID, at, actor) {
		return
	}
	fresh, msg := s.liveRunForExec(ctx, run.ID, forwardLiveRunWords)
	if msg != "" {
		fail(map[string]any{"port": port, "error": msg})
		writeErrorReason(w, http.StatusConflict, reasonRunForwardUnavailable, msg)
		return
	}
	if fresh.MachineKind.IsMacOSVM() && guestagent.MacScreenSharingPort(port) {
		writeErrorReason(w, http.StatusForbidden, reasonRunForwardPortRefused, attachForwardScreenMsg)
		return
	}
	// The CLI's pre-check: every check above, and nothing thawed, held or started.
	if r.URL.Query().Get("check") == "1" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if msg := s.thawForCommand(ctx, fresh, actor); msg != "" {
		fail(map[string]any{"port": port, "error": msg})
		writeErrorReason(w, http.StatusConflict, reasonRunForwardUnavailable, msg)
		return
	}
	if !s.sshAcquireSession(run.ID) {
		fail(map[string]any{"port": port, "error": fmt.Sprintf("%s (max %d)", sshCapReasonChannels, s.cfg.SSHMaxSessionsPerRun)})
		writeErrorReason(w, http.StatusTooManyRequests, reasonRunForwardBusy, fmt.Sprintf(attachForwardBusyMsg, s.cfg.SSHMaxSessionsPerRun))
		return
	}
	defer s.sshReleaseSession(run.ID)

	// The forward's own context: the client's end, the re-check and the ping each end it with their cause, and the
	// bridge then closes the relay.
	chCtx, chCancel := context.WithCancelCause(ctx)
	defer chCancel(errSSHChannelClosed)
	sess, err := s.execFresh(chCtx, run.ID, forwardLiveRunWords, runner.ExecSpec{Argv: sshRelayArgv(fresh.MachineKind, port)})
	if err != nil {
		if errors.Is(err, runner.ErrExecStreamUnsupported) {
			fail(map[string]any{"port": port, "error": runExecUnsupportedMsg})
			writeErrorReason(w, http.StatusNotImplemented, reasonRunInspectExecStreamUnsupported, runExecUnsupportedMsg)
			return
		}
		failed := loggedMsg(ctx, attachForwardFailedMsg, err)
		fail(map[string]any{"port": port, "error": failed})
		writeErrorReason(w, http.StatusBadGateway, reasonRunForwardFailed, failed)
		return
	}
	// The origin was decided above (see handleAttachWS: InsecureSkipVerify says the caller checked it).
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		// Accept wrote its own HTTP error. The relay is closed, and the failed start is audited as the others are.
		if sess.Close != nil {
			_ = sess.Close()
		}
		fail(map[string]any{"port": port, "error": attachForwardFailedMsg})
		return
	}
	defer c.CloseNow()

	// A WebSocket has no half-close, so a read error is the client's end (or the context's: the first cause wins).
	// Ending chCtx makes the bridge close the session, as an SSH channel's close does.
	conn := websocket.NetConn(chCtx, c, websocket.MessageBinary)
	parks := &forwardParks{}
	ch := wsForwardChannel{Conn: conn, gone: chCancel, parks: parks}
	go s.attachForwardPing(chCtx, c, parks, chCancel)

	// The re-check: the ticket's credential and the entry rule (attachAuthority), the run's state and its data_out
	// rule. The first failure ends the forward with its own cause; a read that fails ends it too (fail closed).
	base := s.attachAuthority(r, run.ID)
	var endCause atomic.Pointer[error]
	stop := func(cause error) error {
		endCause.Store(&cause)
		return errAttachAuthorityEnded
	}
	check := s.attachForwardRecheck(run.ID, base, stop)
	s.holdAttachAuthority(chCtx, check, func() { chCancel(*endCause.Load()) })

	var end bridgeEnd
	exit, _, bytesOut := s.sshBridgeExecSession(chCtx, run.ID, actor, ch, noteBridgeEnd(chCtx, sess, &end), false)

	extra := map[string]any{"port": port, "bytes": bytesOut}
	outcome := bridgeOutcome(exit, end, "the forward relay", extra)
	if words, ended := attachForwardEndWords(context.Cause(chCtx)); ended {
		extra["error"], outcome = words, "failure"
	}
	// BaseCtx, not the request's: the row must outlive the connection's teardown (sshgateway_channels.go's forward).
	s.recordStreamAudit(s.cfg.BaseCtx, fresh.SandboxRef, s.attachForwardEvent(run.ID, at, actor, outcome, extra))
}

// refuseForwardDataOut is an SSH direct-tcpip channel's data_out gate for this door: a rule that cannot be read is
// blocked, and any rule but allowed refuses the forward with the run.data_out.deny row SSH writes. True when it has
// answered.
func (s *Server) refuseForwardDataOut(w http.ResponseWriter, r *http.Request, runID uuid.UUID, at types.ActorType, actor string) bool {
	ctx := r.Context()
	rule, _, err := s.runDataOutRule(ctx, runID)
	if err != nil {
		rule = types.TransferBlocked
	}
	if rule.DataOut() == types.TransferAllowed {
		return false
	}
	s.auditDataOutDenied(ctx, runID, at, actor, "port_forward", rule)
	writeErrorReason(w, http.StatusConflict, reasonDataOutBlocked, fmt.Sprintf(attachForwardDataOutMsg, rule))
	return true
}

// attachForwardRecheck is the check an open forward runs every attachAuthorityEvery: base (the ticket's credential
// and the entry rule, nil when attachAuthority has nothing to check), then the run's state, then its data_out rule.
// Each failure goes through stop with the cause the forward's row words; a read that fails is its own cause, so the
// row never says the run ended when it could not be read.
func (s *Server) attachForwardRecheck(runID uuid.UUID, base func(context.Context) error, stop func(error) error) func(context.Context) error {
	return func(lctx context.Context) error {
		if base != nil {
			if err := base(lctx); err != nil {
				// attachAuthority's own verdict is "session ended"; any other error is a read it could not make.
				if errors.Is(err, errAttachAuthorityEnded) {
					return stop(errAttachAuthorityEnded)
				}
				return stop(errAttachForwardReadFailed)
			}
		}
		// liveRunForExec answers notFound on any GetRun error, so that answer covers a failed store read too.
		if _, msg := s.liveRunForExec(lctx, runID, forwardLiveRunWords); msg != "" {
			if msg == forwardLiveRunWords.notFound {
				return stop(errAttachForwardReadFailed)
			}
			return stop(errAttachForwardRunEnded)
		}
		rule, _, err := s.runDataOutRule(lctx, runID)
		if err != nil {
			return stop(errAttachForwardReadFailed)
		}
		if rule.DataOut() != types.TransferAllowed {
			return stop(errAttachForwardDataOut)
		}
		return nil
	}
}

// attachForwardEndWords is the row's error for the cause that ended an open forward, and false for the client's own
// end (errSSHChannelClosed), whose outcome bridgeOutcome decides.
func attachForwardEndWords(cause error) (string, bool) {
	switch {
	case errors.Is(cause, errAttachAuthorityEnded):
		return attachForwardEndSession, true
	case errors.Is(cause, errAttachForwardRunEnded):
		return attachForwardEndRun, true
	case errors.Is(cause, errAttachForwardDataOut):
		return attachForwardEndDataOut, true
	case errors.Is(cause, errAttachForwardReadFailed):
		return attachForwardEndReadFailed, true
	case errors.Is(cause, errAttachForwardPing):
		return attachForwardEndPing, true
	}
	return "", false
}

// attachForwardPort reads the port query: decimal digits only, from 1 to 65535.
func attachForwardPort(q string) (int, bool) {
	if q == "" || strings.Trim(q, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(q)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// attachForwardEvent is the one place the run.port.forward row is built.
func (s *Server) attachForwardEvent(runID uuid.UUID, at types.ActorType, actor, outcome string, data map[string]any) types.AuditEvent {
	return s.auditEvent(&runID, at, actor, "run.port.forward", runID.String(), outcome, mustJSON(data))
}

// wsForwardChannel is a forward's WebSocket as the ssh.Channel sshBridgeExecSession bridges: its bytes both ways, no
// stderr (the relay's stderr is dropped, as a direct-tcpip channel's is) and no channel requests. A read error is the
// client's end (a WebSocket has no half-close), so it ends the forward's context, which closes the relay. parks tells
// the ping loop when either copy waits on a write.
type wsForwardChannel struct {
	net.Conn
	gone  context.CancelCauseFunc
	parks *forwardParks
}

// Read is the client-to-sandbox copy's read. The copy is free while inside it, and parked from the moment it returns
// bytes until its next Read: that span is the copy's write into the sandbox's stdin (and the presence mark,
// presenceReader.Read).
func (c wsForwardChannel) Read(p []byte) (int, error) {
	c.parks.reader.free()
	n, err := c.Conn.Read(p)
	if err != nil {
		c.gone(errSSHChannelClosed)
		return n, err
	}
	c.parks.reader.park()
	return n, nil
}

// Write is the sandbox-to-client copy's write, parked while inside it.
func (c wsForwardChannel) Write(p []byte) (int, error) {
	c.parks.writer.park()
	defer c.parks.writer.free()
	return c.Conn.Write(p)
}

func (wsForwardChannel) CloseWrite() error                              { return nil }
func (wsForwardChannel) SendRequest(string, bool, []byte) (bool, error) { return false, nil }
func (wsForwardChannel) Stderr() io.ReadWriter                          { return discardReadWriter{} }

// discardReadWriter is a stderr that reads nothing and keeps nothing.
type discardReadWriter struct{}

func (discardReadWriter) Read([]byte) (int, error)    { return 0, io.EOF }
func (discardReadWriter) Write(p []byte) (int, error) { return len(p), nil }

// forwardEpoch and forwardNow give the parks a monotonic clock; forwardNow is never 0, which a park reads as free.
var forwardEpoch = time.Now()

func forwardNow() int64 { return int64(time.Since(forwardEpoch)) + 1 }

// forwardPark records when one copy of a forward last waited on a write, so the ping loop can leave out a window
// in which a missing pong may be that wait rather than a gone peer. One goroutine moves a park; the loop reads it.
type forwardPark struct {
	since atomic.Int64 // forwardNow when the current wait began; 0 while the copy is free
	freed atomic.Int64 // forwardNow when the last wait ended
}

func (p *forwardPark) park() { p.since.Store(forwardNow()) }

func (p *forwardPark) free() {
	if p.since.Load() != 0 {
		p.freed.Store(forwardNow()) // before since, so a reader that sees since == 0 sees this freed
		p.since.Store(0)
	}
}

// parkedSince reports whether the copy waited on a write at any time from t to now.
func (p *forwardPark) parkedSince(t int64) bool { return p.since.Load() != 0 || p.freed.Load() >= t }

// forwardParks are a forward's two copies. reader is the client-to-sandbox copy, parked from the moment a read
// returns bytes until its next read: its write into the sandbox's stdin. writer is the sandbox-to-client copy,
// parked while inside its write to the socket.
type forwardParks struct{ reader, writer forwardPark }

func (p *forwardParks) parkedSince(t int64) bool {
	return p.reader.parkedSince(t) || p.writer.parkedSince(t)
}

// attachForwardPing reaps a gone peer. It pings every attachPingEvery; a ping timeout ends the forward only on the
// ping's own deadline, with the forward still live, when neither copy waited on a write from that ping's tick to
// its deadline. A tick whose previous window had a wait sends no ping: the client's writer may still be waiting,
// and its pong would then fail its own read.
func (s *Server) attachForwardPing(ctx context.Context, c *websocket.Conn, parks *forwardParks, end context.CancelCauseFunc) {
	every := s.attachPingEvery()
	t := time.NewTicker(every)
	defer t.Stop()
	last := forwardNow()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tick := forwardNow()
		skip := parks.parkedSince(last)
		last = tick
		if skip {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, every)
		err := c.Ping(pctx)
		pErr := pctx.Err()
		cancel()
		if errors.Is(err, net.ErrClosed) {
			return // the socket closed: the read path records that end
		}
		if forwardPingCounts(err, pErr, ctx.Err(), parks.parkedSince(tick)) {
			end(errAttachForwardPing)
			return
		}
	}
}

// forwardPingCounts reports whether a ping's result ends the forward as a ping timeout: an error on the ping's own
// deadline (pErr), with the forward still live (ctxErr nil), not a closed socket, and no wait on a write in the
// ping's window. Anything else is not a ping timeout: a closed socket, the control write's own 5 s wait while this
// server's writer holds the lock, or the forward's own end.
func forwardPingCounts(err, pErr, ctxErr error, parked bool) bool {
	return err != nil && !errors.Is(err, net.ErrClosed) && errors.Is(pErr, context.DeadlineExceeded) && ctxErr == nil && !parked
}
