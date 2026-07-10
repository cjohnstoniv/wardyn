// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// attachReadBuf is the PTY read chunk size. Terminal output is bursty and small;
// a modest buffer keeps latency low without large per-frame allocations.
const attachReadBuf = 32 * 1024

// attachKeepaliveInterval is how often, while a human is attached, the handler
// bumps the run's updated_at so the idle reaper (which measures idleness by
// agent_runs.updated_at) does not stop an actively-attached session. Client
// input ALSO bumps it immediately; the ticker covers a session that is open but
// momentarily silent (e.g. watching long-running output).
const attachKeepaliveInterval = 30 * time.Second

// attachWriteTimeout bounds a single server->client frame write so a stuck
// client socket cannot wedge the read pump forever.
const attachWriteTimeout = 30 * time.Second

// resizeMsg is the only control message the client may send out-of-band on the
// PTY stream: a window-size change. Everything else on the client->server
// direction is raw PTY input (binary frames). Resize is sent as a TEXT frame so
// it is unambiguously distinct from binary keystroke bytes.
type resizeMsg struct {
	Type string `json:"type"` // "resize"
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
//  6. Keepalive: periodically (and on client input) TouchRun so the reaper
//     leaves an actively-attached run alone.
//  7. Emit session.attach on open and session.detach on close.
//
// SECURITY (invariants 3 & 4):
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

	// FIX #8 + N1 (defense-in-depth): in LOCAL no-auth mode the attach WS is
	// protected only by same-origin at websocket.Accept. Reject BEFORE the upgrade
	// so the socket is never opened. Two gates, same as the REST surface in
	// humanOrAdminAuth: (1) a non-loopback TCP peer (a direct LAN client forging
	// "Host: 127.0.0.1" against a 0.0.0.0 bind) and (2) a non-loopback Host (browser
	// DNS-rebinding, which arrives from a loopback peer). SSO/token modes already
	// require a credential, so this is local-mode-only.
	if s.cfg.LocalMode && !s.cfg.LocalTrustForwarder && !isLoopbackRemoteAddr(r.RemoteAddr) {
		writeError(w, http.StatusForbidden, "local mode: request peer is not loopback (bind wardynd to 127.0.0.1, set WARDYN_LOCAL_TRUST_FORWARDER when behind a loopback-only publish, or configure auth)")
		return
	}
	if s.cfg.LocalMode && !isLoopbackHost(r.Host) {
		writeError(w, http.StatusForbidden, "local mode: request Host is not loopback (DNS-rebinding guard)")
		return
	}

	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}

	// A runner must be wired to attach to anything (headless API mode cannot).
	if s.cfg.Runner == nil {
		writeError(w, http.StatusServiceUnavailable, "no runner configured; attach unavailable")
		return
	}

	run, err := s.cfg.Store.GetRun(ctx, id)
	if notFoundIf(w, err, "run") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get run: "+err.Error())
		return
	}

	// FAIL CLOSED before upgrading: only a RUNNING run with a live sandbox ref
	// can be attached. Rejecting here (plain HTTP) keeps a bad attach a clean
	// error rather than a WebSocket that opens and immediately dies.
	if run.State != types.RunRunning {
		writeError(w, http.StatusConflict, "run is not RUNNING; cannot attach (state="+string(run.State)+")")
		return
	}
	if run.SandboxRef == "" {
		writeError(w, http.StatusConflict, "run has no sandbox; cannot attach")
		return
	}

	// Initial PTY size from optional query params (?cols=&rows=); the client may
	// also resize later via a control message. Defaults (0) let the driver pick.
	opts := runner.AttachOptions{
		Cols: parseUint16(r.URL.Query().Get("cols")),
		Rows: parseUint16(r.URL.Query().Get("rows")),
	}

	principalType, principal := actorFromRequest(r)

	// Upgrade to WebSocket. SAME-ORIGIN is enforced: InsecureSkipVerify is left
	// false (the zero value) so coder/websocket rejects cross-origin upgrades
	// whose Origin host does not match the Host header. We do NOT set
	// OriginPatterns: the default (empty patterns + InsecureSkipVerify=false)
	// means "only allow same-origin", which is exactly what we want — the UI is
	// served from the SAME origin as this API (see mountUI), so browser attach
	// works, while a hostile page on another origin cannot drive the socket.
	// This is the conservative choice; a reverse-proxy deployment that serves the
	// UI from a different host would add that host to OriginPatterns here.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: false,
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

	// Open the interactive shell inside the sandbox. On failure, close the socket
	// cleanly with a policy-violation status and audit the failed attach.
	sess, err := s.cfg.Runner.Attach(ctx, run.SandboxRef, opts)
	if err != nil {
		s.recordAudit(ctx, s.auditEvent(&id, principalType, principal, "session.attach",
			id.String(), "failure", mustJSON(map[string]any{"error": err.Error()})))
		_ = c.Close(websocket.StatusInternalError, "attach failed")
		return
	}
	defer sess.Close() // tears down ONLY the exec stream, never the sandbox.

	s.recordAudit(ctx, s.auditEvent(&id, principalType, principal, "session.attach",
		id.String(), "success", mustJSON(map[string]any{
			"sandbox_ref": run.SandboxRef, "cols": opts.Cols, "rows": opts.Rows,
		})))

	// PROVENANCE (PIECE 3): record this interactive session as a replayable
	// asciicast so the human-in-sandbox is in the audit trail. We tee the server
	// -> client PTY OUTPUT (what appeared on the terminal) through a re-snapshotting
	// masker (liveMaskWriter, same MaskRegistry recording.go uses) into a v2
	// asciicast builder. The cast is persisted on session close, keyed per
	// run+session (a session suffix) so concurrent/sequential attaches never
	// clobber each other or the batch run's cast (keyed by bare runID).
	//
	// LIMITATIONS (honest): (1) only the OUTPUT direction is recorded, not
	// keystroke input — this matches asciinema's "o" event model and the existing
	// player. (2) Masking is verbatim-only (the documented secretmask residual:
	// base64/hex/narrated secrets are not caught); a secret split across two writes
	// IS now masked (liveMaskWriter retains a cross-write tail). (3) The cast is buffered in
	// memory for the session's lifetime and written once at close, which is fine
	// for human-length interactive sessions but is not a streaming sink.
	sessionID := uuid.New().String()
	castTee, finishRecording := s.newSessionRecorder(id, sessionID, opts)

	// A keepalive ping bumps updated_at while attached. We run it on a context
	// derived from the request so it stops when the handler returns.
	pumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Touch immediately so an attach that arrives just before a reap tick still
	// resets the idle clock.
	_ = s.cfg.Store.TouchRun(pumpCtx, id)
	go s.attachKeepalive(pumpCtx, id)

	// Bidirectional pump. closeReason is filled by whichever side ends first.
	// castTee (may be nil when no RecordingStore is wired) receives a copy of the
	// masked PTY output for the asciicast.
	closeReason := s.attachPump(pumpCtx, c, sess, id, castTee)
	cancel()

	// Persist the recording (best-effort) and emit session.recording when one was
	// actually written. finishRecording is a no-op when recording is disabled.
	// Use the daemon-lifetime BaseCtx (not the request ctx, which is typically
	// cancelled the instant the WebSocket closes) so the provenance write + audit
	// are not lost to a cancelled context at detach time.
	finishCtx := s.cfg.BaseCtx
	if finishCtx == nil {
		finishCtx = context.Background()
	}
	finishRecording(finishCtx, principalType, principal)

	// session.detach on close (always emitted, even on error pumps).
	//
	// FINDING (medium, fixed): this audit was recorded on the request ctx, which
	// the WebSocket layer cancels the instant the socket closes — exactly when
	// detach happens — so the detach audit was routinely dropped (a hole in the
	// attribution trail). Record it on finishCtx (the daemon-lifetime BaseCtx /
	// background fallback computed above), so the close event is durably written
	// even though the request context is already cancelled.
	s.recordAudit(finishCtx, s.auditEvent(&id, principalType, principal, "session.detach",
		id.String(), "success", mustJSON(map[string]any{"reason": closeReason})))

	// Best-effort clean close; the deferred CloseNow is the fail-closed backstop.
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// attachKeepalive periodically bumps the run's updated_at so the idle reaper
// does not stop a session a human is actively attached to. It runs until ctx is
// cancelled (the pump ended / the handler returned). A TouchRun error is benign
// here — the worst case is the reaper sees the run as idle, which the never-reap
// policy escape hatch covers for long unattended sessions.
func (s *Server) attachKeepalive(ctx context.Context, id uuid.UUID) {
	t := time.NewTicker(attachKeepaliveInterval)
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

// attachPump runs the two halves of the PTY relay and returns a short reason for
// the side that ended first. It launches Session.Read -> client in a goroutine
// and runs client -> Session.Write (plus resize control) on the caller's
// goroutine. When either half ends it cancels the shared context so the other
// half unblocks and the function returns.
//
// castTee, when non-nil, receives a copy of EVERY chunk of PTY output (after it
// is written to the client) for the session recording. It is the masked
// asciicast sink. A castTee write error never affects the live session — the
// recording is best-effort provenance, not part of the data path.
func (s *Server) attachPump(ctx context.Context, c *websocket.Conn, sess runner.Session, id uuid.UUID, castTee io.Writer) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	reasonCh := make(chan string, 2)

	// Session.Read -> client (binary PTY frames). The blocking sess.Read is run
	// on its own goroutine; cancelling ctx (via the writer half ending) closes
	// the session in the deferred sess.Close of the handler, which unblocks Read
	// with an error, so this goroutine exits.
	go func() {
		buf := make([]byte, attachReadBuf)
		for {
			n, rerr := sess.Read(buf)
			if n > 0 {
				wctx, wcancel := context.WithTimeout(ctx, attachWriteTimeout)
				werr := c.Write(wctx, websocket.MessageBinary, buf[:n])
				wcancel()
				if werr != nil {
					reasonCh <- "client write failed"
					cancel()
					return
				}
				// Tee the output into the session recording (best-effort: a
				// recording write error must not break the live terminal).
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

	// client -> Session.Write, with TEXT frames decoded as resize control.
	go func() {
		for {
			typ, data, rerr := c.Read(ctx)
			if rerr != nil {
				if websocket.CloseStatus(rerr) != -1 {
					reasonCh <- "client closed"
				} else {
					reasonCh <- "client read error"
				}
				cancel()
				return
			}
			// Any client traffic counts as activity: keep the session alive.
			_ = s.cfg.Store.TouchRun(ctx, id)

			switch typ {
			case websocket.MessageText:
				// Control channel: only resize is understood. An unparseable or
				// unknown control message is ignored (it is never injected into
				// the PTY, so it cannot smuggle keystrokes).
				var msg resizeMsg
				if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
					_ = sess.Resize(ctx, msg.Cols, msg.Rows)
				}
			case websocket.MessageBinary:
				// Raw PTY input (keystrokes).
				if _, werr := sess.Write(data); werr != nil {
					reasonCh <- "session write failed"
					cancel()
					return
				}
			}
		}
	}()

	<-ctx.Done()
	// Drain a reason if one is available; otherwise the parent ctx was cancelled.
	select {
	case reason := <-reasonCh:
		return reason
	default:
		return "context cancelled"
	}
}

// newSessionRecorder builds the interactive-session recording pipeline and
// returns (tee, finish):
//
//   - tee is the io.Writer the attach pump feeds PTY OUTPUT into. It is the
//     front of: liveMaskWriter (re-snapshotting secret masking with a retained
//     cross-write tail) -> CastWriter (asciicast v2) -> an in-memory buffer. When
//     no RecordingStore is configured tee is nil and finish is a no-op, so attach
//     works unchanged in headless/no-store mode.
//   - finish flushes the masker tail and persists the buffered asciicast to the
//     RecordingStore under a per-run+session key (so it never clobbers the batch
//     run's cast or a concurrent attach), then emits a session.recording audit
//     event. It is best-effort: a persist failure is audited as a failure but
//     never fails the detach.
//
// SECURITY: the recording is masked against the MaskRegistry (invariant 1: no
// verbatim secret leakage into the recording). Unlike the agent UPLOAD path
// (which snapshots once at run end, after every mint is registered), an attach
// is live, so the masker RE-SNAPSHOTS the registry on each write to catch a
// credential minted mid-session. When MaskRegistry is nil it is a safe
// pass-through (documented residual unchanged).
func (s *Server) newSessionRecorder(runID uuid.UUID, sessionID string, opts runner.AttachOptions) (io.Writer, func(ctx context.Context, principalType types.ActorType, principal string)) {
	noop := func(context.Context, types.ActorType, string) {}
	if s.cfg.RecordingStore == nil {
		return nil, noop
	}

	buf := &bytes.Buffer{}
	cast := recording.NewCastWriter(buf, int(opts.Cols), int(opts.Rows), s.cfg.Now().UTC())

	// Mask the OUTPUT before it lands in the cast, RE-SNAPSHOTTING the registry on
	// every write so a credential minted MID-SESSION (after attach start) is masked
	// too, AND retaining a cross-write tail (the trailing bytes that form an
	// in-progress secret) so a secret whose verbatim bytes straddle two adjacent
	// ~32 KiB PTY chunks is reassembled and masked (FIX #12) — the same
	// split-secret defense the brokered-upload path's MaskingWriter provides. The
	// per-run secret set is tiny so per-write masking is cheap. A nil registry /
	// empty snapshot is a pass-through (the asciicast is still well-formed).
	mw := &liveMaskWriter{reg: s.cfg.MaskRegistry, runID: runID, dst: cast}

	finish := func(ctx context.Context, principalType types.ActorType, principal string) {
		// FIX #12 + #13: take the masker lock across (a) flushing the retained tail
		// into the cast and (b) reading the recording buffer, so a secret sitting in
		// the tail at session end is still masked (not dropped or leaked) and the
		// buffer is never read while the attach Read pump is mid-write. mw.mu is the
		// single mutex serialising the whole mask -> cast -> buf advance, so the read
		// below cannot race a concurrent Write from the pump goroutine.
		mw.mu.Lock()
		mw.flushLocked()
		had := cast.HadOutput()
		// Copy under the lock; the pump may keep appending after we release it.
		snap := append([]byte(nil), buf.Bytes()...)
		mw.mu.Unlock()

		// Skip persisting a header-only (no output) cast: an attach that produced
		// no terminal output is not worth an empty replay artifact.
		if !had {
			return
		}

		key := recording.CastKey(runID.String(), sessionID)
		err := s.cfg.RecordingStore.SaveCastNamed(ctx, runID.String(), sessionID, bytes.NewReader(snap))
		outcome := "success"
		data := map[string]any{"session": sessionID, "key": key, "bytes": len(snap)}
		if err != nil {
			outcome = "failure"
			data["error"] = err.Error()
		}
		s.recordAudit(ctx, s.auditEvent(&runID, principalType, principal, "session.recording",
			key, outcome, mustJSON(data)))
	}

	return mw, finish
}

// liveMaskWriter masks PTY output before it lands in the interactive-session
// cast. It combines two properties the persisted recording needs (invariant 1):
//
//   - RE-SNAPSHOT: it re-reads the per-run secret set on every write, so a
//     credential minted MID-SESSION (e.g. the broker mints a token while a human
//     is attached) is masked, not only secrets registered at attach start. The
//     sibling upload path (recording.go) can snapshot once because it runs at run
//     END; an attach is live, so it must re-snapshot.
//   - TAIL RETENTION (FIX #12): it withholds the trailing bytes that form a strict
//     prefix of a registered secret (see pendingTailLen) and prepends them to the
//     next chunk before masking, so a secret whose verbatim bytes straddle two
//     adjacent PTY writes is reassembled and masked rather than written through
//     unmasked. This provides the same split-secret defense as
//     secretmask.MaskingWriter; that writer cannot be reused directly because it
//     binds a fixed Masker, whereas an attach must re-snapshot (see above). The
//     strict-prefix tail is tighter than MaskingWriter's blanket (maxLen-1) tail:
//     it never withholds already-safe output, so there is no added latency.
//
// mu serialises Write against the tail flush + buffer read in finishRecording, so
// the attach Read pump (writer) and detach (reader/flush) never race the shared
// tail or recording buffer (FIX #13). A nil registry is a pass-through.
type liveMaskWriter struct {
	mu    sync.Mutex
	reg   *secretmask.Registry
	runID uuid.UUID
	dst   io.Writer
	tail  []byte // withheld (already-masked) bytes carried to the next write
}

func (w *liveMaskWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	snap := w.reg.Snapshot(w.runID)
	// Prepend the withheld tail so a secret straddling the previous/this write is
	// reassembled before masking. Re-masking already-masked tail bytes is a no-op
	// (the placeholder contains no secret).
	buf := append(w.tail, p...) //nolint:gocritic // intentional tail+chunk join
	masked := secretmask.NewMasker(snap).Mask(buf)

	// Withhold only the trailing bytes that are a genuine in-progress secret (a
	// strict prefix of some registered secret) so a secret split across this write
	// and the next is reassembled and masked. Unlike a blind (maxLen-1) tail, this
	// forwards everything else immediately: no latency and no withholding of
	// already-safe output.
	tailLen := pendingTailLen(masked, snap)
	forward := masked[:len(masked)-tailLen]
	// Copy the retained tail off the shared backing array before it is reused.
	w.tail = append([]byte(nil), masked[len(masked)-tailLen:]...)

	if len(forward) > 0 {
		if _, err := w.dst.Write(forward); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// flushLocked emits any withheld tail (re-masked) so the trailing bytes held back
// as a possible in-progress secret are not dropped from the cast at session end.
// A dangling tail is only ever a strict (incomplete) secret prefix — a full
// secret would have matched and been replaced during Write — so flushing it does
// not leak a secret. The caller MUST hold w.mu.
func (w *liveMaskWriter) flushLocked() {
	if len(w.tail) == 0 {
		return
	}
	masked := secretmask.NewMasker(w.reg.Snapshot(w.runID)).Mask(w.tail)
	w.tail = nil
	_, _ = w.dst.Write(masked)
}

// pendingTailLen returns how many trailing bytes of masked must be withheld
// because they form a strict prefix of some registered secret and could complete
// into a full secret on the next write. Returns 0 when no trailing bytes are
// mid-secret, so a chunk with no dangling partial is flushed immediately. Only
// secrets of at least secretmask.MinLen bytes are considered (the set NewMasker
// actually masks). O(secrets × maxLen) per write; the per-run secret set is tiny.
func pendingTailLen(masked []byte, secrets [][]byte) int {
	best := 0
	for _, s := range secrets {
		if len(s) < secretmask.MinLen {
			continue
		}
		// Longest k in [best+1, min(len(s)-1, len(masked))] with the last k bytes of
		// masked equal to the first k bytes of s. k < len(s) keeps it a STRICT prefix
		// (a full match was already replaced by Mask, so it never remains here).
		maxK := len(s) - 1
		if maxK > len(masked) {
			maxK = len(masked)
		}
		for k := maxK; k > best; k-- {
			if bytes.Equal(masked[len(masked)-k:], s[:k]) {
				best = k
				break
			}
		}
	}
	return best
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
