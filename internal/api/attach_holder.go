// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Attach-session holder registry + audited take-over.
//
// THE PROBLEM THIS EXISTS TO FIX: attach is a SHARED tmux session. handleAttachWS
// (attach.go) opens a fresh Runner.Attach per client against the same persistent
// session, so opening the run page while a `wardyn attach` holds it from a CLI
// means two clients silently compete for one PTY — and neither can observe the
// other. The Redraw button in ui/src/app/components/attach-terminal.tsx exists
// only to clean up the tmux clamp that competition leaves behind.
//
// The honest version: name the holder, admit the second client READ-ONLY, and
// make displacing them an audited act.
package api

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/google/uuid"

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
// displaced client receives. THE CONTRACT WITH THE UI: a take-over closes the
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

// attachHolder is the client currently holding a run's shared tmux PTY: the one
// whose keystrokes reach the terminal. A second client is admitted as a
// read-only observer and is NOT recorded here — the registry names the WRITER,
// which is the only thing a take-over needs to displace and the only thing the
// "who has this terminal" question is really asking.
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

	// evicted flips the instant a take-over removes this holder from the
	// registry, and it is what actually REVOKES write authority.
	//
	// THE BUG THIS FIXES: the pumps gate writes on the *attachHolder pointer
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

// canWrite reports whether this holder may still drive the PTY. A nil holder is
// a read-only observer (never registered); an evicted one was displaced by a
// take-over and must stop writing AT EVICTION, not whenever its socket happens
// to finish dying.
func (h *attachHolder) canWrite() bool { return h != nil && !h.evicted.Load() }

func (h *attachHolder) setSize(cols, rows uint16) {
	h.mu.Lock()
	h.cols, h.rows = cols, rows
	h.mu.Unlock()
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
// THE CONTRACT THE UI IMPLEMENTS AGAINST:
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

// attachHolderRegistry is the per-daemon map of run id -> current PTY holder.
//
// CEILING: it is IN-PROCESS. A multi-replica control plane sees only its OWN
// replica's holders, so "held:false" means "nobody is attached THROUGH THIS
// DAEMON" — the UI copy must not claim more than that. Wardyn refuses
// replicas>1 by construction today (deployment.yaml, same assumption as
// Server.siteConfigMu and secretmask.Registry), so this is exact, not hopeful.
// ponytail: in-process holder registry, single-daemon truth. Upgrade path is a
// store row keyed by run id (holder principal + since + source + a heartbeat to
// expire a holder whose replica died) if wardynd ever runs multi-replica.
type attachHolderRegistry struct {
	mu      sync.Mutex
	holders map[uuid.UUID]*attachHolder
}

// The registry lives on Server (server.go's attachHolders field), beside
// sshSessions and lastTouch — the same per-process, per-run bookkeeping shape
// this package already uses twice. It was briefly a package-level sync.Map
// keyed by *Server instead; that kept the mechanism in one file but leaked one
// entry per Server for the process's lifetime (a test binary builds many and
// the map has no eviction), and it put a global where the house pattern is a
// struct field.
func (s *Server) attachRegistry() *attachHolderRegistry { return &s.attachHolders }

// registerAttachHolder claims runID's PTY for h.
//
// readOnly=true means somebody ELSE already holds it: h was NOT registered and
// the caller is an observer — it streams output and its input is dropped
// server-side (see attachPump / sshShellPump).
//
// release MUST be deferred by the caller, not called inline next to its
// session.detach audit: a panicking pump would otherwise strand a phantom
// holder that every later attach reads as "held" forever, and only a restart
// would clear it. release is idempotent and identity-checked — a holder that
// was already displaced by a take-over (and replaced by a fresh attach) never
// evicts its successor.
func (s *Server) registerAttachHolder(runID uuid.UUID, h *attachHolder) (readOnly bool, release func()) {
	reg := s.attachRegistry()
	reg.mu.Lock()
	_, held := reg.holders[runID]
	if !held {
		// Lazily built so the zero Server is ready to use (every other
		// per-run map on Server does the same). Reads elsewhere in this file
		// need no such guard — a nil map reads as empty.
		if reg.holders == nil {
			reg.holders = map[uuid.UUID]*attachHolder{}
		}
		reg.holders[runID] = h
	}
	reg.mu.Unlock()

	if held {
		return true, func() {} // observer: nothing was registered, nothing to release
	}
	return false, func() {
		reg.mu.Lock()
		if reg.holders[runID] == h {
			delete(reg.holders, runID)
		}
		reg.mu.Unlock()
	}
}

// attachHolderFor returns runID's current holder, or nil when nobody holds it.
func (s *Server) attachHolderFor(runID uuid.UUID) *attachHolder {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.holders[runID]
}

// evictAttachHolder removes and returns runID's holder in one atomic step, so
// two concurrent take-overs displace exactly one session between them (the
// loser gets nil and reports "nobody is attached") instead of both auditing a
// take-over of the same human.
func (s *Server) evictAttachHolder(runID uuid.UUID) *attachHolder {
	reg := s.attachRegistry()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	h := reg.holders[runID]
	delete(reg.holders, runID)
	if h != nil {
		// Revoke write authority HERE, under the same lock that removes the
		// entry, so it takes effect the moment the take-over is decided rather
		// than whenever the displaced socket finishes closing. See the field's
		// doc for the window this closes.
		h.evicted.Store(true)
	}
	return h
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
//	200 {"taken_over":true,"previous_holder":"alice@example.com","previous_source":"web"}
//	409 nobody is attached (taking over nothing is a client bug worth surfacing)
//
// Same owner-or-admin gate as the read above.
//
// The take-over EVICTS; it does not promote. The caller's own read-only socket
// (if it has one) stays read-only and must RECONNECT to claim the writer slot —
// which costs the UI one reconnect it already knows how to do, and avoids
// inventing a mid-stream "you may now type" state machine on both ends. Between
// the eviction and that reconnect the registry reports held:false, which is
// true: nobody holds it.
func (s *Server) handleAttachTakeover(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	if _, ok := s.getRunAuthorized(w, r, id); !ok {
		return
	}

	prev := s.evictAttachHolder(id)
	if prev == nil {
		writeError(w, http.StatusConflict, "nobody is attached to this run; nothing to take over")
		return
	}

	actorType, principal := actorFromRequest(r)
	// AUDIT FIRST, DISPLACE SECOND. The displaced session's own teardown is what
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

	writeJSON(w, http.StatusOK, map[string]any{
		"taken_over":      true,
		"previous_holder": prev.principal,
		"previous_source": prev.source,
	})
}
