// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The local transport of the attach WebSocket (GET /runs/{id}/attach?transport=local&local_nonce=):
// the person's CLI on the run's own runner holds the terminal over that runner's local socket, and
// this WebSocket is the control half. Every check handleAttachWS makes runs first, so a local attach
// is refused exactly where a relay one is (C1). The organisation keeps the holder registry and the
// writer lease, the recording (the runner copies the output on pty_tee and the recorder here masks
// it), the audit rows and the ending: every ender closes this socket and resets pty_tee, and the
// runner then ends the PTY. Off unless WARDYN_ATTACH_LOCAL is on.
const (
	attachTransportLocal = "local"
	// attachLocalGrantFailedCode closes a local socket whose runner did not take the grant (the CLI's
	// nonce was unknown or lapsed, or the call failed): a transport answer the CLI retries once over
	// the relay with a new ticket.
	attachLocalGrantFailedCode   websocket.StatusCode = 4409
	attachLocalGrantFailedReason                      = "attach_local_grant_failed"
	// attachTeeStalledReason ends a local session whose recording copy this server could not
	// acknowledge: the runner shows no more than its window ahead of the last acknowledgement.
	attachTeeStalledReason = "attach_tee_stalled"
	// attachTeeAckTimeout bounds one acknowledgement, inside the runner's window timeout.
	attachTeeAckTimeout = runnerwire.LocalAttachTeeWindowTimeout / 2
)

// The detail of a 409 attach_local_unavailable: why this run cannot be attached on its runner's own
// computer. Not a refusal of the attach: the relay may still serve it.
const (
	localAttachFlagOff       = "flag_off"
	localAttachNotYourRunner = "not_your_runner"
	localAttachRunnerTooOld  = "runner_too_old"
	localAttachRunnerOffline = "runner_offline"
)

// localAttachSub is the part of a runner's substrate a local attach uses.
type localAttachSub interface {
	runner.LocalAttacher
	Caps() (runnerwire.Caps, bool)
}

// localAttachSubFor is runner id's substrate (the test seam localAttachSubs replaces it).
func (s *Server) localAttachSubFor(id uuid.UUID) localAttachSub {
	if s.localAttachSubs != nil {
		return s.localAttachSubs(id)
	}
	return s.runnerEndpoint(id).sub
}

// promotionCovered is maskCovered for a local observer becoming the writer (the test seam
// localCovered replaces it: a run losing coverage mid-session without mutating Config).
func (s *Server) promotionCovered(ctx context.Context, runID uuid.UUID) bool {
	if s.localCovered != nil {
		return s.localCovered(runID)
	}
	return s.maskCovered(ctx, runID)
}

// localGrantReason is the fixed code a failed grant is audited under. The runner's own text is never
// written: a refusal may quote the nonce it was given, which is for the runner alone.
func localGrantReason(err error) string {
	var we *runnerwire.Error
	if errors.As(err, &we) && we.Code.Valid() {
		return string(we.Code)
	}
	return "local_attach_refused"
}

// localAttachTarget is the runner a local attach is granted on.
type localAttachTarget struct {
	runnerID uuid.UUID
	sub      localAttachSub
}

// localAttachTargetFor checks the local preconditions, after every check of handleAttachWS has
// passed, and answers the 409 itself when one fails (ok false). It writes no authz.denied row: the
// answer is a transport's, and the CLI asks the relay next. Only a person's own ticket on that
// person's own run on that person's own runner is local: never break-glass (no ticket, local mode,
// an operator-owned run), never a sponsor, never an organisation's runner (C4).
func (s *Server) localAttachTargetFor(w http.ResponseWriter, r *http.Request, run types.AgentRun) (localAttachTarget, bool) {
	t, detail := s.localAttachCheck(r, run)
	if detail == "" {
		return t, true
	}
	writeJSON(w, http.StatusConflict, errorBody{Reason: reasonAttachLocalUnavailable, Detail: detail,
		Error: "this run cannot be attached on its runner's own computer (" + detail + "); attach through the organisation instead"})
	return t, false
}

func (s *Server) localAttachCheck(r *http.Request, run types.AgentRun) (localAttachTarget, string) {
	if !s.cfg.AttachLocal {
		return localAttachTarget{}, localAttachFlagOff
	}
	ta, ok := ticketActorFromContext(r.Context())
	if !ok || s.cfg.LocalMode || run.OperatorOwned || ta.actorType != types.ActorHuman || ta.principal == "" ||
		ta.principal != run.CreatedBy || run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return localAttachTarget{}, localAttachNotYourRunner
	}
	rs, ok := s.cfg.Store.(interface {
		GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error)
	})
	if !ok {
		return localAttachTarget{}, localAttachNotYourRunner
	}
	rn, err := rs.GetRunner(r.Context(), *run.RunnerID)
	if err != nil || rn.State != types.RunnerClaimed || rn.OwnerKind != types.RunnerOwnerPerson || rn.Owner != ta.principal {
		return localAttachTarget{}, localAttachNotYourRunner
	}
	sub := s.localAttachSubFor(rn.ID)
	caps, reported := sub.Caps()
	if !reported || !s.runnerOnline(rn.ID) {
		return localAttachTarget{}, localAttachRunnerOffline
	}
	if !caps.LocalAttach {
		return localAttachTarget{}, localAttachRunnerTooOld
	}
	return localAttachTarget{runnerID: rn.ID, sub: sub}, ""
}

// localAttach is one local session's handler state, beside the socket's webAttach.
type localAttach struct {
	wa     *webAttach
	target localAttachTarget
	nonce  string

	mu       sync.Mutex
	sess     runner.LocalSession // nil until the runner takes the grant
	observer bool                // the mode the runner last applied

	pushMu  sync.Mutex    // one mode push at a time
	kick    chan struct{} // the holder's demoted signal
	reasonC chan string   // the first ender's reason
	ended   atomic.Bool   // an ender ran: no more mode pushes
}

// localAttachRequested is localAttachTargetFor when the socket asks for transport=local, else nil
// and ok.
func (s *Server) localAttachRequested(w http.ResponseWriter, r *http.Request, run types.AgentRun) (*localAttachTarget, bool) {
	if r.URL.Query().Get("transport") != attachTransportLocal {
		return nil, true
	}
	t, ok := s.localAttachTargetFor(w, r, run)
	return &t, ok
}

// newLocalAttach makes wa's holder a local one granted on t (nil for a relay socket, which gets nil).
func newLocalAttach(wa *webAttach, t *localAttachTarget, nonce string) *localAttach {
	if t == nil {
		return nil
	}
	wa.fields = map[string]any{"transport": attachTransportLocal, "runner_id": t.runnerID.String()}
	la := &localAttach{wa: wa, target: *t, nonce: nonce, kick: make(chan struct{}, 1), reasonC: make(chan string, 1)}
	h := wa.holder
	h.source, h.notify, h.displace, h.demoted = attachSourceLocal, la.notify, la.displace, la.kick
	return la
}

// pump serves the socket until it ends and returns the close reason: the relay pump, or for a local
// session (la set) its control half, the bytes being on the runner's computer.
func (wa *webAttach) pump(la *localAttach, castTee io.Writer) string {
	if la != nil {
		return la.run(castTee)
	}
	return wa.s.attachPump(wa.pumpCtx, wa.c, wa.run.ID, wa.holder, castTee, wa.open)
}

// run grants the session on the runner and serves it until the socket ends; it returns the close
// reason. castTee nil is an unrecorded session: the runner then sends no copy and gets no acks.
func (la *localAttach) run(castTee io.Writer) string {
	wa, h, s := la.wa, la.wa.holder, la.wa.s
	ctx := wa.pumpCtx
	observer := !h.canWrite()
	opts := s.attachOptionsFor(wa.run.ID, h, !observer)
	gctx, cancel := context.WithTimeout(ctx, s.attachPrepareEvery())
	sess, err := la.target.sub.LocalAttach(gctx, wa.run.SandboxRef, runner.LocalAttachRequest{Nonce: la.nonce, Options: opts, Tee: castTee != nil})
	cancel()
	if ctx.Err() != nil || h.evicted.Load() {
		// Ended some other way (a client close, a take-over, an ender) while the grant was in flight.
		if err == nil {
			_ = sess.Close()
		}
		wa.audit("failure", map[string]any{"state": "attaching", "error": errAttachCancelled.Error()})
		return la.reason("attach cancelled")
	}
	if err != nil {
		wa.audit("failure", map[string]any{"state": "attaching", "error": localGrantReason(err)})
		_ = wa.c.Close(attachLocalGrantFailedCode, attachLocalGrantFailedReason)
		return attachLocalGrantFailedReason
	}
	la.mu.Lock()
	la.sess, la.observer = sess, observer
	la.mu.Unlock()
	// Every ender cancels ctx, and that resets pty_tee: the runner ends the PTY.
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	defer func() { _ = sess.Close() }()
	h.ready.setReadyIf(func() bool { return true })
	wa.audit("success", map[string]any{"sandbox_ref": wa.run.SandboxRef, "state": "ready", "read_only": observer})
	wa.modeMu.Lock()
	_ = writeAttachModeAs(ctx, wa.c, observer, s.attachHolderFor(wa.run.ID), attachTransportLocal)
	wa.modeMu.Unlock()

	go la.tee(sess, castTee)
	go la.read()
	go la.ping()
	go la.fence()
	<-ctx.Done()
	return la.reason("context cancelled")
}

// reason is the first ender's reason, else fallback.
func (la *localAttach) reason(fallback string) string {
	select {
	case r := <-la.reasonC:
		return r
	default:
		return fallback
	}
}

// end records reason (unless an earlier ender did), resets pty_tee and closes the socket with code.
// The close frame goes first, as everywhere on this socket; cancel is the backstop.
func (la *localAttach) end(code websocket.StatusCode, reason string) {
	la.note(reason)
	la.resetTee()
	go func() {
		_ = la.wa.c.Close(code, reason)
		la.wa.cancel()
	}()
}

// stop ends the session from its own side (the client or the runner went away).
func (la *localAttach) stop(reason string) {
	la.note(reason)
	la.wa.cancel()
}

func (la *localAttach) note(reason string) {
	la.ended.Store(true)
	select {
	case la.reasonC <- reason:
	default:
	}
}

func (la *localAttach) resetTee() {
	la.mu.Lock()
	sess := la.sess
	la.mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
}

// displace is attachHolder.displace for a local socket: pty_tee is reset at once, so the runner
// ends the PTY without waiting for the close handshake.
func (la *localAttach) displace(reason string) {
	la.note(reason)
	la.resetTee()
	la.wa.displace(reason)
}

// notify is attachHolder.notify for a local socket. A promotion is pushed to the runner (sync); a
// writer's resize re-sends an observer its frame, as on the relay.
func (la *localAttach) notify(readOnly bool, h *attachHolder) {
	if !readOnly {
		la.sync()
		return
	}
	la.wa.modeMu.Lock()
	defer la.wa.modeMu.Unlock()
	if !la.wa.holder.canWrite() {
		_ = writeAttachModeAs(la.wa.pumpCtx, la.wa.c, true, h, attachTransportLocal)
	}
}

// sync pushes the holder's write authority to the runner when it differs from what the runner last
// applied: on a promotion, a demotion and every lease-fence tick. A writer is re-proved covered by
// the masking corpus first, as the relay re-proves it before re-opening a writer exec; an uncovered
// run is not promoted and nothing is sent. A push the runner does not acknowledge in time ends the
// session (it may have been left in the other mode).
func (la *localAttach) sync() {
	la.pushMu.Lock()
	defer la.pushMu.Unlock()
	wa, h, s := la.wa, la.wa.holder, la.wa.s
	ctx := wa.pumpCtx
	la.mu.Lock()
	sess, last := la.sess, la.observer
	la.mu.Unlock()
	if sess == nil || ctx.Err() != nil || la.ended.Load() {
		return
	}
	observer := !h.canWrite()
	if observer == last {
		return
	}
	var cols, rows uint16
	if !observer {
		if !s.promotionCovered(ctx, wa.run.ID) {
			la.ended.Store(true) // wa.fail closes the socket
			la.resetTee()
			wa.fail(errAttachUncovered, true)
			return
		}
		opts := s.attachOptionsFor(wa.run.ID, h, true)
		cols, rows = opts.Cols, opts.Rows
	}
	pctx, cancel := context.WithTimeout(ctx, attachLeaseCallTimeout)
	err := sess.SetObserver(pctx, observer, cols, rows)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			la.end(websocket.StatusPolicyViolation, attachLeaseLostReason)
		}
		return
	}
	la.mu.Lock()
	la.observer = observer
	la.mu.Unlock()
	wa.modeMu.Lock()
	_ = writeAttachModeAs(ctx, wa.c, observer, s.attachHolderFor(wa.run.ID), attachTransportLocal)
	wa.modeMu.Unlock()
}

// fence evaluates the holder's write authority on every attachLeaseFenceEvery tick and on every
// demotion, and pushes each change (sync).
func (la *localAttach) fence() {
	t := time.NewTicker(attachLeaseFenceEvery)
	defer t.Stop()
	for {
		select {
		case <-la.wa.pumpCtx.Done():
			return
		case <-t.C:
		case <-la.kick:
		}
		la.sync()
	}
}

// tee reads the runner's copy of the output. With a recorder each chunk goes to it, and only then
// is its end offered for acknowledgement; without one the bytes are dropped and nothing is acked.
func (la *localAttach) tee(sess runner.LocalSession, castTee io.Writer) {
	var ack *teeAcker
	if castTee != nil {
		ack = &teeAcker{wake: make(chan struct{}, 1)}
		go ack.run(la.wa.pumpCtx, sess, func() { la.end(websocket.StatusInternalError, attachTeeStalledReason) })
	}
	buf := make([]byte, attachReadBuf)
	var accepted int64
	for {
		n, err := sess.Read(buf)
		if n > 0 {
			la.wa.holder.lastOutput.Store(time.Now().UnixNano())
			if castTee != nil {
				if _, werr := castTee.Write(buf[:n]); werr == nil {
					accepted += int64(n)
					ack.offer(accepted)
				}
			}
		}
		if err != nil {
			// A mode push the runner did not acknowledge resets the tee itself; wait for that push
			// to record its own reason before naming this one.
			la.pushMu.Lock()
			//lint:ignore SA2001 a barrier, not a critical section
			la.pushMu.Unlock() //nolint:staticcheck // a barrier, not a critical section
			// An ender that reset the tee closes the socket itself: cancelling here would tear the
			// connection down before its close frame, and the CLI would read a bare drop.
			if !la.ended.Load() {
				la.stop("runner ended the session")
			}
			return
		}
	}
}

// teeAcker acknowledges the tee: one acknowledgement in flight, always carrying the newest offset
// the recorder accepted, so acks are bounded by the round trip and never queued per chunk.
type teeAcker struct {
	latest atomic.Int64
	wake   chan struct{}
}

func (a *teeAcker) offer(offset int64) {
	a.latest.Store(offset)
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// run sends acknowledgements until ctx ends. A refused or late one (a full call table, a stalled
// link) is not retried: stalled ends the session, failing closed.
func (a *teeAcker) run(ctx context.Context, sess runner.LocalSession, stalled func()) {
	var sent int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		}
		off := a.latest.Load()
		if off <= sent {
			continue
		}
		actx, cancel := context.WithTimeout(ctx, attachTeeAckTimeout)
		err := sess.AckTee(actx, off)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				stalled()
			}
			return
		}
		sent = off // an offer made meanwhile left a wake behind: the newest offset goes next
	}
}

// localControl is a client->server control frame on a local socket: a writer's presence (the CLI
// sends one at most every 10 s while the person types) or its window size. Terminal bytes never
// travel on this socket; a binary frame is dropped.
type localControl struct {
	Type string `json:"type"` // "presence" or "resize"
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

func (la *localAttach) read() {
	h := la.wa.holder
	for {
		typ, data, err := la.wa.c.Read(la.wa.pumpCtx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 {
				la.stop("client closed")
			} else {
				la.stop("client read error")
			}
			return
		}
		var msg localControl
		if typ != websocket.MessageText || json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch {
		case msg.Type == "presence" && h.canWrite() && h.onInput != nil:
			h.onInput()
		case msg.Type == "resize" && msg.Cols > 0 && msg.Rows > 0:
			h.setSize(msg.Cols, msg.Rows)
			if h.canWrite() {
				h.recordResize(msg.Cols, msg.Rows)
			}
		}
	}
}

// ping probes the control socket as the relay pump probes its own (attachPingInterval).
func (la *localAttach) ping() {
	every := la.wa.s.attachPingEvery()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-la.wa.pumpCtx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(la.wa.pumpCtx, every)
		err := la.wa.c.Ping(pctx)
		cancel()
		if err != nil {
			la.stop("ping timeout")
			return
		}
	}
}
