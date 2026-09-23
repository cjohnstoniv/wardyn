// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Attach-session holder registry + audited take-over.
//
// The problem this exists to fix: attach is a SHARED tmux session. handleAttachWS
// (attach.go) opens a fresh Runner.Attach per client against the same persistent
// session, so opening the run page while a `wardyn attach` holds it from a CLI
// means two clients silently compete for one PTY — and neither can observe the
// other. The Redraw button in ui/src/app/components/attach-terminal.tsx exists
// only to clean up the tmux clamp that competition leaves behind.
//
// The honest version: name the holder, admit the second client READ-ONLY, hand
// it the PTY in place when the holder leaves, and make displacing them an
// audited act.
package api

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The two transports that can hold a run's PTY, as reported by GET
// /runs/{id}/attach-holder and the attach-mode control frame. "ssh" is not
// decoration: a CLI holder over the SSH gateway (sshgateway_channels.go) is
// invisible to the browser unless it registers here too, and a browser that
// confidently reports "nobody is attached" while somebody is typing is worse
// than one that reports nothing at all.
const (
	attachSourceWeb = "web"
	attachSourceSSH = "ssh"
)

// attachTakeoverReasonPrefix is the load-bearing half of the close reason a
// displaced client receives. The contract with the UI: a take-over closes the
// displaced WebSocket with code 1008 (websocket.StatusPolicyViolation) and a
// reason of exactly `taken over by <principal>`. The UI matches on that code
// (or this prefix) and must NOT take its bounded-reconnect path — a displaced
// client that reconnects lands straight back on top of the new holder, which is
// the two-clients-fighting state this whole file exists to end. Every OTHER
// close (1000 clean, 1006 abnormal, any network blip) keeps its existing
// reconnect behaviour untouched.
const attachTakeoverReasonPrefix = "taken over by "

// wsCloseReasonMax is RFC 6455 §5.5's hard cap on a close frame's reason: the
// whole control frame payload is 125 bytes, 2 of which are the status code. A
// longer reason is not truncated by the library — coder/websocket REFUSES to
// send the frame at all — so a long principal (an SSO sub, an email) would
// silently cost the displaced client the very reason it needs to distinguish a
// take-over from a network drop. attachTakeoverReason enforces it.
const wsCloseReasonMax = 123

// attachHolder is ONE client attached to a run's shared tmux PTY — the writer
// whose keystrokes reach the terminal, or a read-only observer queued behind
// it. The two are the same type because an observer becomes the writer in
// place, on the socket it already has, the moment the writer leaves: writable
// says which one it is right now, and "who has this terminal" is answered by
// the registry's writer slot, never by the object's type.
type attachHolder struct {
	principal string
	actorType types.ActorType
	since     time.Time
	source    string // attachSourceWeb | attachSourceSSH

	// displace ends this holder's session with a reason the DISPLACED CLIENT can
	// read, and is deliberately a closure rather than the bare context.CancelFunc
	// the two transports share: how you deliver "you were taken over" is
	// transport-specific (a WebSocket close frame vs a line on the SSH channel's
	// stderr), and on the WebSocket lane the ORDER is load-bearing — cancelling
	// the pump context first makes coder/websocket tear the connection down
	// abruptly, and an abruptly-killed socket delivers no close frame at all, so
	// the client would see a bare drop and reconnect into the fight. It must not
	// block its caller (the take-over HTTP request); both implementations hand
	// the teardown to a goroutine.
	displace func(reason string)

	// mu guards the LIVE geometry only. cols/rows are updated from the resize
	// control frames the pumps already handle, because the handshake value goes
	// stale the first time the operator drags their window — and a confidently
	// reported stale geometry is worse than none. Everything above is immutable
	// after construction, so it needs no lock.
	mu   sync.Mutex
	cols uint16
	rows uint16

	// writable is THIS client's write authority, and it is also the OBSERVER
	// MARKER. It used to be "holder == nil": both pumps nulled their own holder
	// for an observer, so a promoted observer had no object left to flip and
	// the only way to get the keyboard was to reconnect. Moving the marker onto
	// the client makes promotion one atomic store under the registry lock, and
	// every write and resize — which already funnel through canWrite — changes
	// answer together.
	//
	// atomic, not mu: the pumps read it per chunk and must never contend with a
	// resize write.
	writable atomic.Bool

	// notify tells THIS client its mode changed — today only read-only ->
	// writable, on promotion. Transport-specific (a second attach-mode frame on
	// the WebSocket, the client's own geometry plus a stderr line on the SSH
	// channel) and NEVER called with the registry lock held: a client write can
	// block for the full attachWriteTimeout, which would stall every other
	// attach on the daemon behind one unresponsive peer. The registry hands the
	// caller a closure to run on its own goroutine instead (releaseAttach).
	notify func(readOnly bool, holder *attachHolder)

	// evicted flips the instant a take-over removes this holder from the
	// registry, and it is what actually REVOKES write authority.
	//
	// The bug this fixes: the pumps gate writes on the *attachHolder pointer
	// they captured at attach time. Removing the entry from the registry map
	// does not touch that pointer, and displace() deliberately closes the
	// socket rather than cancelling the pump context (a cancelled ctx sends no
	// close frame, so the client would see a bare drop and reconnect into the
	// fight). But coder/websocket's Close does a full handshake — writeClose
	// then waitCloseHandshake, each with its own multi-second budget, the
	// second of which blocks on the read mutex the displaced pump is holding.
	// So between "take-over returned 200" and "the displaced socket finally
	// dies" there was a window, bounded only by those timeouts against an
	// unresponsive peer, in which the OLD client still wrote into the same tmux
	// session as the new one. Two writers is the exact state this file exists
	// to end, and the gate was asking the client's socket to cooperate.
	//
	// The SSH lane never had the window: its displace() calls cancel() and the
	// pump returns immediately. That asymmetry was the tell.
	//
	// atomic, not mu: the pumps read it on every frame and must never contend
	// with a resize write.
	evicted atomic.Bool
}

// canWrite reports whether this holder may still drive the PTY: it must hold
// the write slot (writable — an observer never did, a promoted observer does)
// and must not have been displaced (evicted AT EVICTION, not whenever its
// socket happens to finish dying). A nil holder is nobody and never writes.
func (h *attachHolder) canWrite() bool {
	return h != nil && h.writable.Load() && !h.evicted.Load()
}

// attachWriteChunk bounds ONE Session.Write issued on a client's behalf, and so
// is the granularity at which write authority is re-tested.
//
// The bug this bounds: canWrite was read once per FRAME and the whole frame was
// then handed to the sandbox. A web frame is one message up to attachReadLimit
// (1 MiB — a paste, deliberately raised for exactly that), and the runner's
// Write blocks under PTY back-pressure, so the window was "however long tmux
// takes to drain the paste", not nanoseconds. A take-over decided and AUDITED
// inside that window still had the displaced human's paste landing in the same
// tmux session the new holder was told they own. 4 KiB is a pipe buffer's worth:
// small enough that the residual after an eviction is one chunk, large enough
// that a paste is not syscall-bound.
const attachWriteChunk = 4 * 1024

// writeGated writes p into sess in attachWriteChunk pieces, re-testing write
// authority before EACH piece, and stops the instant this holder is evicted. It
// is the ONE client->PTY write path both pumps use (attachPump and
// sshShellPump), so the two transports sharing this registry cannot drift on the
// gate the way their resize gates did.
//
// A nil holder is a read-only observer: nothing is written at all.
//
// Ceiling: the chunk already handed to the sandbox cannot be recalled — a
// take-over landing mid-chunk still delivers that chunk. Bounding the residual
// to attachWriteChunk is the honest guarantee; making it exactly zero means
// tearing the runner exec down at eviction (docker's hijacked write aborts on
// Close), which also kills the displaced client's OUTPUT mid-frame.
// ponytail: chunk loop, no queue and no writer goroutine — the upgrade path is
// that exec teardown, if one chunk is ever one too many.
func (h *attachHolder) writeGated(sess runner.Session, p []byte) error {
	for len(p) > 0 {
		if !h.canWrite() {
			return nil // evicted, or an observer: drop the rest, server-side
		}
		n := min(len(p), attachWriteChunk)
		if _, err := sess.Write(p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

func (h *attachHolder) setSize(cols, rows uint16) {
	h.mu.Lock()
	h.cols, h.rows = cols, rows
	h.mu.Unlock()
}

// size is the client's own last-known geometry, which a promoted observer has
// to re-apply: it never resized the shared tmux window while it was watching
// (that would clamp the writer's terminal), so the window it inherits is the
// departed writer's.
func (h *attachHolder) size() (cols, rows uint16) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cols, h.rows
}

// attachHolderView is the wire shape for BOTH GET /runs/{id}/attach-holder and
// the holder half of the attach-mode control frame — one Go type, one TS type
// for the UI lane. A nil receiver is the "nobody holds it" answer, so the read
// path never has to branch.
type attachHolderView struct {
	Held      bool       `json:"held"`
	Principal string     `json:"principal,omitempty"`
	Since     *time.Time `json:"since,omitempty"`
	Cols      uint16     `json:"cols,omitempty"`
	Rows      uint16     `json:"rows,omitempty"`
	Source    string     `json:"source,omitempty"` // "web" | "ssh"
}

func (h *attachHolder) view() attachHolderView {
	if h == nil {
		return attachHolderView{Held: false}
	}
	h.mu.Lock()
	cols, rows := h.cols, h.rows
	h.mu.Unlock()
	since := h.since
	return attachHolderView{
		Held:      true,
		Principal: h.principal,
		Since:     &since,
		Cols:      cols,
		Rows:      rows,
		Source:    h.source,
	}
}

// attachModeMsg is the ONE server->client control frame on the attach
// WebSocket, sent as a TEXT frame the instant the socket opens — the mirror of
// the client->server resize frame, using the same split the protocol already
// has (binary = raw PTY bytes, text = control JSON). The client MUST learn it
// is read-only from the server; asking the client to refrain from sending is
// not a control, it is a request to the one component we do not control.
//
// The contract the UI implements against:
//
//	{"type":"attach-mode","read_only":true,
//	 "holder":{"held":true,"principal":"alice@example.com",
//	           "since":"2026-08-16T12:00:00Z","cols":120,"rows":40,"source":"web"}}
//
// It is sent on EVERY connect, read_only=false included (with holder naming the
// caller itself), so the UI never has to infer its mode from silence. holder is
// omitted only in the impossible-in-practice case of a holder that vanished
// between registration and this write. Existing clients are unaffected: the
// current terminal already ignores non-binary frames (attach-terminal.tsx's
// onmessage checks `ev.data instanceof ArrayBuffer`).
type attachModeMsg struct {
	Type     string            `json:"type"` // always "attach-mode"
	ReadOnly bool              `json:"read_only"`
	Holder   *attachHolderView `json:"holder,omitempty"`
}

// writeAttachMode sends the attach-mode frame. Called from the handler
// goroutine BEFORE the pump starts, so it never races the pump's own writer.
func writeAttachMode(ctx context.Context, c *websocket.Conn, readOnly bool, holder *attachHolder) error {
	msg := attachModeMsg{Type: "attach-mode", ReadOnly: readOnly}
	if holder != nil {
		v := holder.view()
		msg.Holder = &v
	}
	wctx, cancel := context.WithTimeout(ctx, attachWriteTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, mustJSON(msg))
}

// attachHolderRegistry is the per-daemon map of run id -> attach state: the
// current PTY writer and the observers queued behind it.
//
// Ceiling: it is IN-PROCESS. A multi-replica control plane sees only its OWN
// replica's holders, so "held:false" means "nobody is attached through this
// daemon" — the UI copy must not claim more than that. Wardyn refuses
// replicas>1 by construction today (deployment.yaml, same assumption as
// Server.siteConfigMu and secretmask.Registry), so this is exact, not hopeful.
// ponytail: in-process holder registry, single-daemon truth. Upgrade path is a
// store row keyed by run id (holder principal + since + source + a heartbeat to
// expire a holder whose replica died) if wardynd ever runs multi-replica.
type attachHolderRegistry struct {
	mu       sync.Mutex
	attaches map[uuid.UUID]*runAttach
}

// runAttach is one run's attach state: the single writer, and the observers
// queued behind it in arrival order. The queue is what makes in-place promotion
// possible at all — while the registry knew only the writer, a writer leaving
// meant "nobody is attached" even with three clients watching, and every one of
// them had to reconnect to get the keyboard.
type runAttach struct {
	writer    *attachHolder
	observers []*attachHolder // oldest first; observers[0] is promoted first
}

// The registry lives on Server (server.go's attachHolders field), beside
// sshSessions and lastTouch — the same per-process, per-run bookkeeping shape
// this package already uses twice. A package-level sync.Map
// keyed by *Server would keep the mechanism in one file but leak one
// entry per Server for the process's lifetime (a test binary builds many and
// the map has no eviction), and would put a global where the house pattern is a
// struct field.
func (s *Server) attachRegistry() *attachHolderRegistry { return &s.attachHolders }

// registerAttachHolder claims runID's PTY for h, or queues h behind the client
// that already holds it.
//
// readOnly=true means h is an OBSERVER: it streams output, its input and its
// resizes are dropped server-side (see attachPump / sshShellPump), and it is
// now IN LINE for the write slot — h.writable flips on this same object the
// moment the writer leaves, with no reconnect and no new socket.
//
// release MUST be deferred by the caller, not called inline next to its
// session.detach audit: a panicking pump would otherwise strand a phantom
// holder that every later attach reads as "held" forever, and only a restart
// would clear it. release is idempotent and identity-checked — a holder that
// was already displaced by a take-over (and replaced by a fresh attach) never
// evicts its successor, and an observer's release only leaves the queue.
//
// release RETURNS the promotion it caused, or nil. Run it on your own
// goroutine (releaseAttach does both in one call): it audits the promotion and
// writes to the PROMOTED client's socket, and that write can block for the full
// attachWriteTimeout — under the registry lock it would stall every other
// attach on the daemon behind one unresponsive peer.
func (s *Server) registerAttachHolder(runID uuid.UUID, h *attachHolder) (readOnly bool, release func() (announce func())) {
	reg := s.attachRegistry()
	reg.mu.Lock()
	// Lazily built so the zero Server is ready to use (every other per-run map
	// on Server does the same). Reads elsewhere in this file need no such
	// guard — a nil map reads as empty.
	if reg.attaches == nil {
		reg.attaches = map[uuid.UUID]*runAttach{}
	}
	ra := reg.attaches[runID]
	if ra == nil {
		ra = &runAttach{}
		reg.attaches[runID] = ra
	}
	if ra.writer == nil {
		ra.writer = h
		h.writable.Store(true)
	} else {
		ra.observers = append(ra.observers, h)
		readOnly = true
	}
	reg.mu.Unlock()

	return readOnly, func() (announce func()) {
		reg.mu.Lock()
		ra := reg.attaches[runID]
		if ra == nil {
			reg.mu.Unlock()
			return nil // already released (release is deferred AND called)
		}
		var promoted *attachHolder
		if ra.writer == h {
			ra.writer = nil
			// FIFO: the oldest observer still on its socket takes the slot.
			// A promoted observer whose socket is ALREADY dead holds it until
			// the pump's ping probe notices — up to about twice
			// attachPingInterval. Named, not fixed: the registry cannot tell a
			// silent peer from an idle one, and the probe already exists.
			if len(ra.observers) > 0 {
				promoted, ra.observers = ra.observers[0], ra.observers[1:]
				ra.writer = promoted
				promoted.writable.Store(true)
			}
		} else {
			ra.observers = slices.DeleteFunc(ra.observers, func(o *attachHolder) bool { return o == h })
		}
		if ra.writer == nil && len(ra.observers) == 0 {
			delete(reg.attaches, runID)
		}
		reg.mu.Unlock()
		return s.announceAttachPromotion(runID, promoted, h.principal)
	}
}

// releaseAttach frees a client's slot and delivers the promotion it caused on
// its own goroutine. One shape, used by both transports, so the two cannot
// drift on it. The goroutine is not optional: the promotion writes to ANOTHER
// client's socket, and a departing session must not wait out a stranger's
// unresponsive peer before recording its own detach.
func releaseAttach(release func() (announce func())) {
	if announce := release(); announce != nil {
		go announce()
	}
}

// announceAttachPromotion is the deferred half of a promotion — the audit row
// and the promoted client's own notice — built under the registry lock and run
// after it is dropped. nil when nothing was promoted, which is the common case.
//
// Audited (session.promote) because write authority over a live sandbox moved
// without anyone asking for it, which is the same reason session.takeover is.
// It grants no NEW capability — every observer passed the same owner-or-admin
// gate when it connected, and a take-over promotes only the taker's own
// socket — but "who could type into this terminal, and from when" has to be
// answerable from the log alone.
func (s *Server) announceAttachPromotion(runID uuid.UUID, promoted *attachHolder, previous string) func() {
	if promoted == nil {
		return nil
	}
	return func() {
		// Daemon-lifetime ctx, never the departing session's: the promotion
		// outlives the release that caused it, and the one row recording that
		// write authority moved must not be dropped with a cancelled context —
		// the same fix the detach and take-over audits carry.
		ctx := s.cfg.BaseCtx
		if ctx == nil {
			ctx = context.Background()
		}
		s.recordAudit(ctx, s.auditEvent(&runID, promoted.actorType, promoted.principal, "session.promote",
			runID.String(), "success", mustJSON(map[string]any{
				"principal":       promoted.principal,
				"source":          promoted.source,
				"previous_holder": previous,
			})))
		if promoted.notify != nil {
			promoted.notify(false, promoted)
		}
	}
}

// attachHolderFor returns runID's WRITER, or nil when nobody holds it — never
// a queued observer. It feeds GET /runs/{id}/attach-holder and the attach-mode
// frame's holder field, and both answer one question: whose keystrokes reach
// this terminal.
func (s *Server) attachHolderFor(runID uuid.UUID) *attachHolder {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if ra := reg.attaches[runID]; ra != nil {
		return ra.writer
	}
	return nil
}

// evictAttachHolderFor removes and returns runID's writer for a take-over by
// `taker`, together with the promotion that take-over caused — in one atomic
// step, so two concurrent take-overs displace exactly one session between them
// (the loser gets nil and reports "nobody is attached") instead of both
// auditing a take-over of the same human.
//
// Only the TAKER'S OWN observer socket is promoted. Promoting the oldest
// bystander instead would hand the terminal — and the audited consequences of
// an act that names the taker — to somebody who never asked for it. A taker
// with no observer socket leaves the slot FREE and reconnects into it, exactly
// as before; queued bystanders stay observers.
func (s *Server) evictAttachHolderFor(runID uuid.UUID, taker string) (prev *attachHolder, announce func()) {
	reg := s.attachRegistry()
	reg.mu.Lock()
	ra := reg.attaches[runID]
	if ra == nil || ra.writer == nil {
		reg.mu.Unlock()
		return nil, nil
	}
	prev, ra.writer = ra.writer, nil
	// Revoke write authority HERE, under the same lock that removes the entry,
	// so it takes effect the moment the take-over is decided rather than
	// whenever the displaced socket finishes closing. See the evicted field's
	// doc for the window this closes.
	prev.evicted.Store(true)

	var promoted *attachHolder
	if taker != "" {
		for i, o := range ra.observers {
			if o.principal != taker {
				continue
			}
			promoted = o
			ra.observers = slices.Delete(ra.observers, i, i+1)
			ra.writer = promoted
			promoted.writable.Store(true)
			break
		}
	}
	if ra.writer == nil && len(ra.observers) == 0 {
		delete(reg.attaches, runID)
	}
	reg.mu.Unlock()
	return prev, s.announceAttachPromotion(runID, promoted, prev.principal)
}

// evictAttachHolder evicts runID's writer and promotes NOBODY. The take-over
// path names the taking principal (evictAttachHolderFor) so it can promote that
// principal's own observer; this is the plain eviction the probes drive
// directly, where there is no taker to attribute a promotion to.
func (s *Server) evictAttachHolder(runID uuid.UUID) *attachHolder {
	prev, _ := s.evictAttachHolderFor(runID, "")
	return prev
}

// attachTakeoverReason builds the close reason the displaced client reads,
// trimmed on a RUNE boundary to RFC 6455's 123-byte cap (see wsCloseReasonMax):
// a mid-rune cut would make the reason invalid UTF-8, which a strict client is
// entitled to reject outright.
func attachTakeoverReason(principal string) string {
	reason := attachTakeoverReasonPrefix + principal
	for len(reason) > wsCloseReasonMax {
		_, size := utf8.DecodeLastRuneInString(reason)
		reason = reason[:len(reason)-size]
	}
	return reason
}

// handleAttachHolder serves GET /api/v1/runs/{id}/attach-holder — "who has this
// run's terminal right now":
//
//	{"held":false}
//	{"held":true,"principal":"alice@example.com","since":"2026-08-16T12:00:00Z",
//	 "cols":120,"rows":40,"source":"web"}
//
// OWNER-OR-ADMIN (getRunAuthorized): a foreign run gets the byte-identical 404 a
// missing run would, so this cannot be used as an existence oracle — or, worse,
// as an "is anyone watching?" oracle over somebody else's run.
func (s *Server) handleAttachHolder(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	if _, ok := s.getRunAuthorized(w, r, id); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.attachHolderFor(id).view())
}

// handleAttachTakeover serves POST /api/v1/runs/{id}/attach/takeover — taking a
// live terminal away from another human, which is why it is audited rather than
// silent:
//
//	200 {"taken_over":true,"previous_holder":"alice@example.com","previous_source":"web","promoted":false}
//	409 nobody is attached (taking over nothing is a client bug worth surfacing)
//
// Same owner-or-admin gate as the read above.
//
// The take-over promotes the caller's OWN read-only socket if it has one (in
// place, no reconnect — see evictAttachHolderFor), and otherwise frees the slot
// for the caller to attach into. It never promotes a bystander. Between an
// unpromoted eviction and the caller's attach the registry reports held:false,
// which is true: nobody holds it.
func (s *Server) handleAttachTakeover(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	// ownsRunOrSuperAdmin, NOT ownsRunOrAdmin: this is the one /runs/{id} route
	// that writes into a live sandbox instead of inspecting or stopping it, so
	// the security tier — refused a ticket, refused the cookie attach lane,
	// stamped `member` on its SSH keys — is refused here too, through the same
	// byte-identical 404. See helpers.go's split.
	if _, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin); !ok {
		return
	}

	actorType, principal := actorFromRequest(r)

	prev, promote := s.evictAttachHolderFor(id, principal)
	if prev == nil {
		writeError(w, http.StatusConflict, "nobody is attached to this run; nothing to take over")
		return
	}
	// Audit first, displace second. The displaced session's own teardown is what
	// makes the reverse order unsafe: see the FINDING on attach.go's
	// session.detach audit, where recording on a context the teardown cancels
	// dropped the event outright. Displacing first means the take-over audit
	// races the very teardown it describes — and the one event that says a human
	// took another human's terminal is exactly the one that must not be lost. An
	// audit written for a displacement that then somehow fails is the safe
	// failure of the two. Daemon-lifetime ctx, NOT r.Context(): the eviction
	// below is irreversible the moment it runs, and a client that aborts the
	// POST mid-flight must not cancel the one write that records who took the
	// terminal — the same fix the four detach/exec/sftp/forward audits carry.
	auditCtx := s.cfg.BaseCtx
	if auditCtx == nil {
		auditCtx = context.Background()
	}
	s.recordAudit(auditCtx, s.auditEvent(&id, actorType, principal, "session.takeover",
		id.String(), "success", mustJSON(map[string]any{
			"previous_holder": prev.principal,
			"previous_source": prev.source,
			"held_since":      prev.since,
		})))

	prev.displace(attachTakeoverReason(principal))
	promoted := promote != nil
	if promote != nil {
		// Off this goroutine and after the displacement: the notice goes to the
		// taker's OTHER socket, whose write can block for attachWriteTimeout,
		// and this HTTP response must not wait on it.
		go promote()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"taken_over":      true,
		"previous_holder": prev.principal,
		"previous_source": prev.source,
		// promoted: the taker already had an observer socket on this run and
		// its first one was flipped to writer IN PLACE (announceAttachPromotion's
		// notify sends that socket a fresh attach-mode frame). A caller whose
		// socket is still open must NOT reconnect on this answer — see
		// doTakeover in attach-terminal.tsx.
		"promoted": promoted,
	})
}
