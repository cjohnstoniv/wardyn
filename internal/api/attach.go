// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// attachReadBuf is the PTY read chunk size. Terminal output is bursty and small;
// a modest buffer keeps latency low without large per-frame allocations.
const attachReadBuf = 32 * 1024

// attachKeepaliveInterval is how often, while a human is attached, the handler
// bumps the run's updated_at so the idle reaper (which measures idleness by
// agent_runs.updated_at) does not stop an actively-attached session. The ticker
// is the ONLY input-independent signal and it is enough: it runs for the whole
// life of the attach, so updated_at is never more than one interval stale and
// the reaper adds exactly that much slack (lifecycle.TouchDebounce). Touching
// per inbound PTY frame instead would put a Postgres UPDATE in front of every
// keystroke and buy nothing the ticker does not already guarantee.
const attachKeepaliveInterval = 30 * time.Second

// attachKeepaliveEvery is attachKeepaliveInterval unless THIS server was built
// with an override (Server.keepaliveEvery — tests only, so the ticker can be
// observed without a 30-second wait).
func (s *Server) attachKeepaliveEvery() time.Duration {
	if s.keepaliveEvery > 0 {
		return s.keepaliveEvery
	}
	return attachKeepaliveInterval
}

// attachWriteTimeout bounds a single server->client frame write so a stuck
// client socket cannot wedge the read pump forever.
const attachWriteTimeout = 30 * time.Second

// attachPauseLimit is how long a client may leave output paused before the pump
// asks it for a pong (an unanswered ping ends the pump): attachWriteTimeout
// unless THIS server was built with an override (Server.pauseLimit, tests only).
func (s *Server) attachPauseLimit() time.Duration {
	if s.pauseLimit > 0 {
		return s.pauseLimit
	}
	return attachWriteTimeout
}

// attachPingInterval is the liveness probe cadence for an otherwise-idle
// attach socket.
//
// The gap this closes: attachWriteTimeout already reaps a stuck peer once
// server->client output is FLOWING — a Write blocks under the timeout and the
// pump ends. It never engages on a SILENT PTY (no output => no Write is ever
// attempted), and the client->server half blocks on c.Read with no deadline of
// its own. MEASURED (attach_holder_test.go,
// TestAttachWS_DeadPeerHolder...): with a quiet shell and a peer that stops
// reading (a laptop lid closed, a network partition — indistinguishable at
// this layer from a client that is merely idle), NOTHING in the existing pump
// frees the holder; it is bounded only by whatever the OS/proxy eventually
// notices about the TCP connection, which can be effectively unbounded. A
// dead holder on a quiet run therefore reads "held" forever to every other
// attacher (browser, `wardyn run attach`, the SSH gateway) until the daemon
// restarts.
//
// c.Ping requires a Read loop already running to observe the pong
// (coder/websocket's own contract) — attachPump's client->server goroutine
// provides exactly that, so this adds no second reader.
//
// Deadline: worst case ~2x this interval before a truly dead peer is reaped
// (one tick to notice the idle window, one full interval waiting for the
// pong). 30s reuses attachWriteTimeout's own budget rather than inventing a
// second "how unresponsive is too unresponsive" number: tighter risks
// evicting a live writer on a slow/lossy link (a mobile hotspot, a laggy
// VPN); looser buys nothing a human notices, since the symptom this exists to
// bound is "stuck read-only forever", not "instantly".
const attachPingInterval = attachWriteTimeout

// attachPingEvery is attachPingInterval unless THIS server was built with an
// override (Server.pingEvery — tests only, mirroring attachKeepaliveEvery).
func (s *Server) attachPingEvery() time.Duration {
	if s.pingEvery > 0 {
		return s.pingEvery
	}
	return attachPingInterval
}

// attachReadLimit bounds ONE client->server message. It must be set explicitly:
// coder/websocket's default is 32 KiB, and exceeding the limit does not drop the
// frame — it CLOSES the socket with StatusMessageTooBig. A terminal paste is a
// single message, so on the default an operator pasting a >32 KiB patch or log
// silently lost their whole session. 1 MiB is far above any realistic paste while
// still bounding what one socket can make the daemon buffer.
const attachReadLimit = 1 << 20

// resizeMsg is the control message the client may send out-of-band on the PTY
// stream: a window-size change, or a pause or resume (Type only) for output flow
// control. Everything else on the client->server
// direction is raw PTY input (binary frames). Resize is sent as a TEXT frame so
// it is unambiguously distinct from binary keystroke bytes.
type resizeMsg struct {
	Type string `json:"type"` // "resize", "pause" or "resume"
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// handleAttachWS is the interactive-attach WebSocket endpoint:
//
//	GET /api/v1/runs/{id}/attach
//
// It is mounted INSIDE the humanOrAdminAuth admin group, so authentication is
// enforced by the group middleware BEFORE this handler ever runs (a valid OIDC
// session or the admin bearer token). The handler therefore never re-checks the
// bearer token; it trusts the middleware and derives the human principal for
// attribution via principalFromRequest.
//
// Flow (fail closed at every step):
//  1. Validate the run id and load the run.
//  2. Reject (HTTP error, BEFORE upgrading) unless the run is RUNNING and has a
//     SandboxRef and a Runner is wired. Failing before the WebSocket upgrade
//     keeps a rejected attach a clean HTTP 4xx/5xx, not a half-open socket.
//  3. Upgrade to WebSocket with same-origin enforcement.
//  4. Runner.Attach opens a fresh interactive shell inside the sandbox.
//  5. Bidirectional pump: client binary frames -> Session.Write; Session.Read
//     -> client binary frames; client TEXT frames -> resize control.
//  6. Keepalive: TouchRun on open and every attachKeepaliveInterval so the
//     reaper leaves an actively-attached run alone.
//  7. Emit session.attach on open and session.detach on close.
//
// Security (invariants 3 & 4):
//   - Invariant 3 (confinement / no new egress): the interactive shell runs
//     INSIDE the existing sandbox via the runner, so it is bounded by exactly
//     the same L0 structural-egress + confinement envelope as the agent. Attach
//     opens NO new network path out of the sandbox — the PTY bytes flow
//     control-plane -> dockerd -> container over the Docker exec hijack, NEVER
//     through the sandbox's HTTP_PROXY egress path. Egress allowlisting and
//     credential minting stay enforced at the proxy/broker; this attach changes
//     none of that.
//   - Invariant 4 (attribution): the caller principal (OIDC sub, LocalMode operator/dev override,
//     or system/admin-token for a bare token caller) is recorded on both the session.attach and
//     session.detach audit events, so an interactive session is attributable to
//     a person, not an anonymous "admin".
func (s *Server) handleAttachWS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Defense-in-depth: in LOCAL no-auth mode the attach WS is
	// protected only by same-origin at websocket.Accept. Reject BEFORE the upgrade
	// so the socket is never opened. Two gates, same as the REST surface in
	// humanOrAdminAuth: (1) a non-loopback TCP peer (a direct LAN client forging
	// "Host: 127.0.0.1" against a 0.0.0.0 bind) and (2) a non-loopback Host (browser
	// DNS-rebinding, which arrives from a loopback peer). SSO/token modes already
	// require a credential, so this is local-mode-only.
	if s.cfg.LocalMode && !s.cfg.LocalTrustForwarder && !isLoopbackRemoteAddr(r.RemoteAddr) {
		writeErrorReason(w, http.StatusForbidden, reasonLocalModePeerNotLoopback, "local mode: request peer is not loopback (bind wardynd to 127.0.0.1, set WARDYN_LOCAL_TRUST_FORWARDER when behind a loopback-only publish, or configure auth)")
		return
	}
	if s.cfg.LocalMode && !isLoopbackHost(r.Host) {
		writeErrorReason(w, http.StatusForbidden, reasonLocalModeHostNotLoopback, "local mode: request Host is not loopback (DNS-rebinding guard)")
		return
	}

	// Same-origin, decided HERE rather than by the library. coder/websocket's
	// own check authorises r.Host and then consults OriginPatterns — which are
	// path.Match GLOBS, so the one extra name an ingress deployment needs could
	// not be expressed as a literal (attachOriginRefused, csrf.go, carries the
	// IPv6 case that made this wrong in both directions). attachOriginRefused
	// makes exactly the comparison the console's CSRF guard makes: r.Host or the
	// host of WARDYN_OIDC_REDIRECT_URL — operator-configured, attacker-
	// unwritable — and nothing else, with an absent Origin allowed for
	// non-browser clients exactly as the library allows it. This is the most
	// dangerous cookie-authenticated capability in the product; it gets ONE
	// extra name, from config, never a wildcard.
	//
	// InsecureSkipVerify on the Accept below says "the caller checked the
	// origin", which is what this block is; leaving it false would re-refuse the
	// ingress host this widening exists for.
	//
	// Decided here, beside the other CALLER gates and above every read of state:
	// it judges who is asking, not what they asked for, so running it
	// after getRunOr404 and the ticket re-check spent a store read on a request
	// that was never going to be served — and could answer a cross-origin
	// upgrade with 404/409, or an authz.denied row, instead of the refusal.
	if s.attachOriginRefused(r) {
		// Audited like the REST guard's two arms (http.go), on the same
		// auth.fail action, reason and actor — a control that refuses
		// silently cannot answer either question an operator has at 3am
		// (csrf.go), and that argument started applying to this socket the
		// moment the decision moved out of the library and into our code.
		s.auditAuthFailedAs(r, csrfActor, csrfAuditReason)
		writeErrorReason(w, http.StatusForbidden, csrfAuditReason, csrfRefusedBody)
		return
	}

	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}

	// A runner must be wired to attach to anything (headless API mode cannot).
	if s.cfg.Runner == nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonAttachNoRunner, "no runner configured; attach unavailable")
		return
	}

	run, ok := s.getRunOr404(w, r, id)
	if !ok {
		return
	}

	// Entry authorization (#1476): the owner rule, on both lanes.
	if s.refuseAttachEntry(w, r, run) {
		return
	}

	// Fail closed before upgrading: only a RUNNING run with a live sandbox ref
	// can be attached. Rejecting here (plain HTTP) keeps a bad attach a clean
	// error rather than a WebSocket that opens and immediately dies.
	if run.State != types.RunRunning {
		writeErrorReason(w, http.StatusConflict, reasonAttachNotRunning, "run is not RUNNING; cannot attach (state="+string(run.State)+")")
		return
	}
	// A kept run is RUNNING with its agent stopped: nothing to attach to.
	if runIsKept(run) {
		writeErrorReason(w, http.StatusConflict, reasonAttachRunKept, "run has ended; cannot attach")
		return
	}
	if run.SandboxRef == "" {
		writeErrorReason(w, http.StatusConflict, reasonAttachNoSandbox, "run has no sandbox; cannot attach")
		return
	}
	// Door 2 of five (mask_manifest.go): the terminal's recorded tail is masked
	// against the run's corpus, which this server must be able to prove whole.
	if s.refuseUncovered(w, r, run.ID, "runs.attach") {
		return
	}

	// Initial PTY size from optional query params (?cols=&rows=). The browser
	// sends its real grid here, so the writer's exec starts at the size it will
	// keep and nothing resizes the shared tmux window after registration.
	opts := runner.AttachOptions{
		Cols: parseUint16(r.URL.Query().Get("cols")),
		Rows: parseUint16(r.URL.Query().Get("rows")),
	}

	principalType, principal := actorFromRequest(r)

	// A paused run is thawed before the exec: the daemon refuses one into a
	// paused container (run_pause.go).
	if err := s.thawForExec(ctx, run, principalType, principal, "presence"); err != nil {
		writeErrorReason(w, http.StatusBadGateway, reasonAttachResumeFailed, "run is paused and could not be resumed; try again")
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		// Accept already wrote an HTTP error response on failure (e.g. a 403 for
		// a cross-origin request); nothing more to do.
		return
	}
	// CloseNow is the fail-closed teardown: it closes the underlying TCP conn
	// without a handshake. A clean close is attempted in the happy path below;
	// this defer guarantees the socket never leaks on any error return.
	defer c.CloseNow()
	// Raise the library's 32 KiB default so a large paste is delivered instead of
	// killing the connection (see attachReadLimit).
	c.SetReadLimit(attachReadLimit)

	// The audit rows this handler writes outlive the request: the WebSocket layer
	// cancels r.Context() the instant the socket closes, which is exactly when
	// the attach outcome and the detach are recorded. Daemon-lifetime BaseCtx
	// (background fallback) carries them, with the portal a delegated ticket was
	// minted through (#1142).
	finishCtx := s.cfg.BaseCtx
	if finishCtx == nil {
		finishCtx = context.Background()
	}
	var via *types.DelegationVia
	if v, ok := audit.DelegationFrom(ctx); ok {
		via = &v
		finishCtx = audit.WithDelegation(finishCtx, v)
	}

	// pumpCtx ends the whole connection: the pump, the exec being opened, the
	// keepalive and the mask-fence watcher all hang off it.
	pumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Holder registry (attach_holder.go). Attach is a SHARED tmux session: a
	// fresh Runner.Attach per client lands on the SAME persistent session, so
	// without this two clients silently compete for one PTY and neither can see
	// the other. Name the holder; admit a second client READ-ONLY (its input is
	// dropped server-side — never by asking the client to refrain); make
	// displacing the holder an audited act (handleAttachTakeover).
	//
	// Registration comes BEFORE the exec exists (attach_exec.go): who writes
	// decides the attach options, so an observer's tmux client is created with
	// ignore-size and never resizes the writer's window.
	//
	// modeMu orders THIS socket's attach-mode frames. The library permits
	// concurrent writes, but not concurrent truth: the opening frame below and
	// a promotion frame from another client's release goroutine can cross, and
	// a client whose LAST frame says read_only:true while the registry has
	// already made it the writer greys out a terminal it is allowed to type
	// into. Both sends take this mutex and read the mode live inside it, so
	// whichever lands second is the one that is still true.
	wa := &webAttach{s: s, c: c, run: run, principalType: principalType, principal: principal,
		finishCtx: finishCtx, pumpCtx: pumpCtx, cancel: cancel}
	holder := &attachHolder{
		principal: principal,
		actorType: principalType,
		since:     s.cfg.Now().UTC(),
		source:    attachSourceWeb,
		onInput:   func() { _ = s.markPresent(ctx, id, principalType, principal, "presence") },
		cols:      opts.Cols,
		rows:      opts.Rows,
		via:       via,
		notify:    wa.notify,
		displace:  wa.displace,
		ping:      c.Ping,
	}
	holder.lastOutput.Store(time.Now().UnixNano())
	wa.holder = holder
	readOnly, releaseHolder := s.registerAttachHolder(id, holder)
	// Deferred as the crash backstop, NOT the release point (see the explicit
	// call right after attachPump returns, below): a panicking pump would
	// otherwise strand a phantom holder that every later attach reads as
	// "held" forever, curable only by a restart. release is idempotent and
	// identity-checked (attach_holder.go), so running it twice is safe.
	defer releaseAttach(releaseHolder)
	if readOnly {
		// Possibly behind its own dead earlier tab: see attach_stale.go.
		go s.probeStaleWriter(pumpCtx, id, holder)
	}
	// Tears down ONLY the exec stream, never the sandbox. Runs before the
	// release above, so a holder never outlives its exec.
	defer func() { _ = holder.mux.Close() }()

	// The first of two session.attach rows: the holder is registered and its exec
	// is being opened. The audit log is append-only and hash-chained, so the
	// outcome is a second row, never an update; this one exists before any
	// take-over can be audited against it.
	s.recordStreamAudit(finishCtx, run.SandboxRef, s.auditEvent(&id, principalType, principal, "session.attach",
		id.String(), "success", mustJSON(map[string]any{
			"sandbox_ref": run.SandboxRef, "cols": opts.Cols, "rows": opts.Rows,
			"state": "attaching", "read_only": readOnly,
		})))

	// An observer KEEPS its holder object: holder.writable is the read-only
	// marker now (attach_holder.go), and nulling it here is exactly what left a
	// promoted observer with nothing to flip. The pump still drops its input
	// and its resizes — both gate on canWrite.
	//
	// Tell the client which mode it got, ALWAYS (read_only=false included) — see
	// attachModeMsg for the exact shape. Written from THIS goroutine, before the
	// pump starts, so it is the first frame the client sees and never races the
	// pump's own writer. A write failure here means the socket is already gone;
	// the pump below ends on its own, so there is nothing to do about it beyond
	// not pretending it succeeded.
	wa.modeMu.Lock()
	_ = writeAttachMode(ctx, c, !holder.canWrite(), s.attachHolderFor(id))
	wa.modeMu.Unlock()

	// Provenance: record this interactive session as a replayable
	// asciicast so the human-in-sandbox is in the audit trail. We tee the server
	// -> client PTY OUTPUT (what appeared on the terminal) through a re-snapshotting
	// masker (liveMaskWriter, same MaskRegistry recording.go uses) into a v2
	// asciicast builder. The cast is persisted on session close, keyed per
	// run+session (a session suffix) so concurrent/sequential attaches never
	// clobber each other or the batch run's cast (keyed by bare runID).
	//
	// Limitations (honest): (1) only the OUTPUT direction is recorded, not
	// keystroke input — this matches asciinema's "o" event model and the existing
	// player. (2) Masking is verbatim-only (the documented secretmask residual:
	// base64/hex/narrated secrets are not caught); a secret split across two writes
	// IS now masked (liveMaskWriter retains a cross-write tail). (3) The cast is buffered in
	// memory for the session's lifetime and written once at close, which is fine
	// for human-length interactive sessions but is not a streaming sink — past
	// maxSessionCastBytes the recording keeps its head and drops the rest, and
	// says so in the session.recording.write audit.
	sessionID := uuid.New().String()
	castTee, finishRecording := s.newSessionRecorder(run, sessionID, opts)

	// Touch immediately so an attach that arrives just before a reap tick still
	// resets the idle clock. The keepalive ping then bumps updated_at while
	// attached, on a context that stops when the handler returns.
	_ = s.cfg.Store.TouchRun(pumpCtx, id)
	go s.attachKeepalive(pumpCtx, id)
	// Started before the exec is opened, so a fence ends an attach in flight too.
	maskFenced := s.endAttachOnMaskFence(pumpCtx, id, c, cancel)

	// Bidirectional pump. closeReason is filled by whichever side ends first.
	// castTee (may be nil when no RecordingStore is wired) receives a copy of the
	// masked PTY output for the asciicast.
	closeReason := s.attachPump(pumpCtx, c, id, holder, castTee, wa.open)
	cancel()
	if maskFenced() {
		closeReason = maskFencedReason
	}
	if wa.failed.Load() {
		closeReason = "attach failed"
	}

	// Free the slot HERE, before the recording persist + the session.detach
	// audit below — not after them, which is where the deferred call above
	// would otherwise leave it (function return, i.e. the very end). A focus-
	// mode remount closes the old attach socket and opens the new one in the
	// same effect flush (canvas.tsx), and the new handshake's
	// registerAttachHolder call was landing inside that old-pump-to-function-
	// return window and being admitted READ-ONLY against its own vanishing
	// self — the reported "sometimes I can never click back in". Releasing the
	// instant the pump ends (persistence and audit are disk/DB I/O with no
	// bound on the holder) shrinks that window to effectively nothing.
	//
	// Ordering this DOES accept: the successor's session.attach can now be
	// recorded before THIS session's session.detach lands (finishRecording's
	// I/O and the audit write below still have to happen). That is the
	// opposite of handleAttachTakeover's "audit first, displace second" rule —
	// deliberately: a take-over is one human forcibly ending another's
	// session, so losing that event is the unacceptable failure. A remount is
	// the SAME principal reclaiming a socket that was always theirs; the audit
	// trail gains an attach slightly ahead of its own detach, never a lost or
	// misattributed event. See TestAttachWS_RemountReleasesHolderBeforeAuditTail
	// for the ordering this pins.
	//
	// releaseAttach, not a bare releaseHolder(): when this client was the
	// writer, its release PROMOTES the oldest observer, and telling that
	// observer so is a write to a FOREIGN socket — never on this goroutine.
	releaseAttach(releaseHolder)

	// Persist the recording (best-effort) and emit session.recording.write when one was
	// actually written. finishRecording is a no-op when recording is disabled.
	// finishCtx is the daemon-lifetime context, so the provenance write + audit
	// are not lost to a cancelled request context at detach time.
	finishRecording(finishCtx, principalType, principal)

	// session.detach on close (always emitted, even on error pumps), on finishCtx
	// for the reason given where it is computed.
	// read_only rides along so the trail distinguishes the human who was DRIVING
	// this terminal from the one who was watching over their shoulder — the pair
	// is otherwise indistinguishable after the fact. It is the LIVE flag, not
	// the mode this socket connected with: an observer promoted mid-session
	// ends as a writer, and recording its connect-time mode would say the human
	// who was driving had only been watching.
	s.recordStreamAudit(finishCtx, run.SandboxRef, s.auditEvent(&id, principalType, principal, "session.detach",
		id.String(), "success", mustJSON(map[string]any{"reason": closeReason, "read_only": !holder.writable.Load()})))

	// Best-effort clean close; the deferred CloseNow is the fail-closed backstop.
	// A failed attach was already closed with its own status (failAttach).
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// attachKeepalive periodically bumps the run's updated_at so the idle reaper
// does not stop a session a human is actively attached to. It runs until ctx is
// cancelled (the pump ended / the handler returned). A TouchRun error is benign
// here — the worst case is the reaper sees the run as idle, which the never-reap
// policy escape hatch covers for long unattended sessions.
func (s *Server) attachKeepalive(ctx context.Context, id uuid.UUID) {
	t := time.NewTicker(s.attachKeepaliveEvery())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.cfg.Store.TouchRun(ctx, id)
		}
	}
}

// parseUint16 parses a decimal string to a uint16, returning 0 on any error
// (the driver then picks a default window size).
func parseUint16(s string) uint16 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}

// refuseAttachEntry is handleAttachWS's entry authorization (#1476): true when
// it has answered. The run's owner, or a super admin on a run with no personal
// owner (mayEnterRun); neither lane may skip it.
//
// Ticket lane: ticketOrHumanAuth's ?ticket= branch runs neither
// humanOrAdminAuth nor requireOperator, so the ticket's OWN stamped
// role/principal (captured at MINT time — see attach_ticket.go) is the only
// signal left, re-checked here against the just-loaded run. An admin stamp
// that fails the rule gets the 403 naming why; a member stamp keeps its own.
// superAdmin is true past this block only on the admin's own run or an
// operator-owned one, so the deny_interactive exemption cannot reach a
// person's run.
func (s *Server) refuseAttachEntry(w http.ResponseWriter, r *http.Request, run types.AgentRun) bool {
	id := run.ID
	if ta, tok := ticketActorFromContext(r.Context()); tok {
		superAdmin := ta.role == oidc.RoleAdmin
		if !mayEnterRun(run, ta.principal, superAdmin) {
			if superAdmin {
				s.auditAttachRefused(r, id, ta.principal, "ticket", runOwnerOnlyReason, ta.via)
				s.refuseRunOwnerOnly(w, r, run)
				return true
			}
			s.auditAttachDenied(r, id, ta.principal, "attach ticket does not authorize this run")
			writeErrorReason(w, http.StatusForbidden, reasonAttachTicketNotYourRun, "attach ticket does not authorize this run")
			return true
		}
		// The fall-through lane is operator-only, and a super admin's ticket
		// is exempt the same way.
		return (!superAdmin || !s.adminDoorExempt(run)) && s.refuseInteractiveAttach(w, r, run)
	}
	if !mayEnterRun(run, principalFromRequest(r), s.isOperator(r.Context())) {
		// The cookie lane: requireOperator and the origin check keep members
		// out of it, and the owner rule keeps a super admin out of a person's
		// run, which that gate alone no longer does.
		s.auditAttachRefused(r, id, principalFromRequest(r), "cookie", runOwnerOnlyReason, nil)
		s.refuseRunOwnerOnly(w, r, run)
		return true
	}
	return s.refuseNoTicketAttach(w, r, run)
}
