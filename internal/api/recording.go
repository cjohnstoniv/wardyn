// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordingAuthorizer is the owner-or-admin authorization callback wired into
// recording.Handler (server.go's Mount call): a run's recording is
// exactly as sensitive as its live PTY attach (it may show whatever the
// agent's injected credentials left on screen), so it gets the SAME
// ownership gate as getRunAuthorized — an admin, or the run's own creator,
// may read it. runIDPrefix is the cast key's run-id portion (Handler has
// already enforced it equals the outer mount {id} before this ever runs); a
// prefix that doesn't parse as a run id, or names no real run, is denied
// (fails closed) rather than erroring — recording.Handler turns any false
// return into the SAME 404 "recording not found" a truly-missing cast gets,
// so this never needs to shape its own response.
// auditRecordingRead is an operator's replay of a recording of a run someone else created.
const auditRecordingRead = "recording.read"

func (s *Server) recordingAuthorizer(r *http.Request, runIDPrefix string) bool {
	id, err := uuid.Parse(runIDPrefix)
	if err != nil {
		return false
	}
	// An operator reads every recording (recordingReader). Replaying another person's session is
	// audited, as attaching to it live is; an owner's own replay is not.
	if s.isOperator(r.Context()) {
		if run, err := s.cfg.Store.GetRun(r.Context(), id); err == nil && run.CreatedBy != principalFromRequest(r) {
			actorType, actor := actorFromRequest(r)
			s.recordAudit(r.Context(), s.auditEvent(&id, actorType, actor, auditRecordingRead, chi.URLParam(r, "runID"), "success", nil))
		}
		return true
	}
	run, err := s.cfg.Store.GetRun(r.Context(), id)
	if err != nil {
		return false
	}
	if s.recordingReader(r, run) {
		return true
	}
	// Audited only once the run is confirmed to genuinely exist — a
	// malformed prefix or an unknown run (both branches above) stays silent;
	// only a POSITIVELY identified foreign run reaches this audit. The 404
	// recording.Handler writes on a false return is unaffected either way.
	s.recordRefusal(r.Context(), r, authz.Deny(authz.ReasonNotOwner, id.String(), "").OnRun(id))
	return false
}

// recordingReader is the one rule for who may read a run's recording, and
// anything quoted from it (projectModelAccessQuote): the run's owner or an
// operator. DELIBERATELY isOperator (three-tier doctrine, internal/auth/oidc's
// RoleSecurityAdmin): a recording is a PRIVACY surface — a replay of
// someone's terminal with their agent's injected credentials on screen.
// Governing policy does not include watching people work, and this is the
// one place ownsRunOrAdmin's incident-response widening must NOT reach.
func (s *Server) recordingReader(r *http.Request, run types.AgentRun) bool {
	return s.isOperator(r.Context()) || run.CreatedBy == principalFromRequest(r)
}

// maxRecordingUploadBytes caps a single recording PUT. An authenticated
// in-sandbox agent could otherwise stream an
// unbounded cast and fill the control plane's disk. 64 MiB comfortably holds a
// long PTY session's asciicast while bounding a single hostile upload. Bytes
// beyond the cap cause http.MaxBytesReader to error, which we surface as 413.
const maxRecordingUploadBytes = 64 << 20 // 64 MiB

// recordingUploadReadWindow bounds how long an upload's body may take to
// arrive once the store starts reading it (the Postgres store first admits the
// save; the file store reads at once), so a slow or stalled client holds a save
// slot for at most this long. It stays below the proxy's 130 s
// control-plane call timeout: an upload admitted within the store's 65 s
// admission wait still answers in 65 + 60 = 125 s, before its proxy gives up.
// 60 s carries a 32 MiB part (where wardyn-rec cuts one) at about 4.5 Mbit/s
// and a 64 MiB upload at about 9 Mbit/s. A body still arriving then fails as
// any read error does: nothing is stored. A var so tests can shorten it.
var recordingUploadReadWindow = 60 * time.Second

// readWindow arms the connection's read deadline at the body's first Read. A
// store that queues saves reads only once it has admitted one, so queue time is
// not charged.
type readWindow struct {
	io.ReadCloser
	rc    *http.ResponseController
	armed bool
}

func (b *readWindow) Read(p []byte) (int, error) {
	if !b.armed {
		b.armed = true
		if err := b.rc.SetReadDeadline(time.Now().Add(recordingUploadReadWindow)); err != nil {
			return 0, err
		}
	}
	return b.ReadCloser.Read(p)
}

// storeUpload masks and stores one upload body as part of runID's cast. A
// connection that cannot take a read deadline is refused, never read without one.
func (s *Server) storeUpload(w http.ResponseWriter, r *http.Request, run types.AgentRun, runID uuid.UUID, part int) error {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("recording upload: connection takes no read deadline: %w", err)
	}

	// Cap the upload before masking; the reader preserves MaxBytesError so an
	// over-cap upload remains a 413 rather than a successful truncated cast. That holds for a cast. A plain
	// log is cut at the masked limit with the server's marker and answered 2xx, and when masking lengthens it
	// so the masked output reaches the limit before the raw stream passes the upload cap, the raw overrun is
	// never read and the cut log is stored all the same.
	limited := http.MaxBytesReader(w, &readWindow{ReadCloser: r.Body, rc: rc}, maxRecordingUploadBytes)

	// PRIMARY masking point: read the upload body through a MaskingWriter so
	// verbatim secret values are replaced with "<secret-hidden>" before the bytes
	// reach the RecordingStore. A nil MaskRegistry is a safe no-op (pass-through).
	//
	// Honest residual: only verbatim byte-identical occurrences are masked.
	// base64-encoded, hex-encoded, or model-narrated representations of secrets
	// are NOT caught. This is intentional — masking catches the most likely
	// accidental leakage vector (token printed to stdout/asciicast).
	//
	// Read only on the store's demand: an early storage failure must not leave
	// an independent goroutine blocked reading the request body.
	body := buildGuardedMaskingBody(limited, s.cfg.MaskRegistry, runID, s.maskGuard(runID))
	if mr, ok := body.(*recordingMaskReader); ok {
		// A recorder that says its body is a plain log is believed over the first-bytes guess below.
		mr.plainLog = r.Header.Get(recording.FormatHeader) == recording.FormatLog
	}
	// One person's uploads queue behind each other before they queue with
	// everyone else's; a run with no person owner queues as itself.
	ctx := r.Context()
	if run.CreatedBy != "" && !run.OperatorOwned {
		ctx = recording.WithAdmissionGroup(ctx, "owner:"+run.CreatedBy)
	}
	if part == 1 {
		return s.cfg.RecordingStore.SaveCast(ctx, runID.String(), body)
	}
	return s.cfg.RecordingStore.SaveCastNamed(ctx, runID.String(), recording.PartSuffix(part), body)
}

// handleUploadRecording accepts a PUT /api/v1/internal/recordings/{runID} from
// wardyn-rec running inside the agent container. The caller must hold a valid
// run token (enforced by internalAuth). The run ID in the path must match the
// sub claim of the token to prevent cross-run pollution.
//
// The request body is the raw asciicast stream. Content-Type is not enforced
// so the fallback .log uploads also work. It is the whole cast of a short run,
// or part 1 of a long one's tail upload (see handleUploadRecordingPart).
func (s *Server) handleUploadRecording(w http.ResponseWriter, r *http.Request) {
	s.saveRecording(w, r, 1)
}

// handleUploadRecordingPart accepts PUT
// /api/v1/internal/recordings/{runID}/parts/{part}: part n >= 2 of a long run's
// cast, which wardyn-rec uploads every 24 h or 32 MiB (RL-12). Each part is
// masked, capped and audited exactly like part 1, and stored under its own key;
// replay joins them (recording.OpenJoined). Part 1 has one address, the bare
// route, so a number that is not canonical decimal >= 2 is a 404; one above
// types.RecordingMaxParts is refused (saveRecording).
func (s *Server) handleUploadRecordingPart(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "part")
	part, err := strconv.Atoi(raw)
	if err != nil || part < 2 || strconv.Itoa(part) != raw {
		writeErrorReason(w, http.StatusNotFound, reasonRecordingInvalidPart, "invalid recording part")
		return
	}
	s.saveRecording(w, r, part)
}

// labelRecordingEvidence marks a recording.upload row as self-reported when the
// run's evidence comes from a self-hosted runner: the cast was produced on a
// developer-administered host. A substrate run's row is left as it was.
func labelRecordingEvidence(run types.AgentRun, fields map[string]any) map[string]any {
	if run.EvidenceSource == types.RunEvidenceRunnerAsserted {
		fields["evidence_source"] = types.RunEvidenceRunnerAsserted
	}
	return fields
}

func (s *Server) saveRecording(w http.ResponseWriter, r *http.Request, part int) {
	// Cross-run guard: the caller must hold the run's OWN token — prevent a
	// token from run A uploading a recording under run B.
	claims, ok := claimsForRunUpload(w, r)
	if !ok {
		return
	}

	// An organisation with recording off still serves the route, behind internalAuth and the relay
	// binding, so a runner that records anyway is told why its upload has nowhere to go.
	if s.cfg.RecordingStore == nil {
		writeErrorReason(w, http.StatusConflict, reasonRecordingOff, "this organisation keeps no session recordings")
		return
	}
	// Door 1 of five (mask_manifest.go): a run whose masking corpus this server
	// cannot prove complete is refused, not passed through. Before the part
	// limit so an uncovered run's refusal never depends on what it uploads.
	if s.refuseUncoveredAgent(w, r, claims, "recordings.upload") {
		return
	}
	// The run internalAuth's liveness gate already read (every part would otherwise read it again); a call
	// that did not come through that gate reads it here.
	run, ok := liveRunFromContext(r.Context())
	if !ok && s.cfg.Store != nil {
		var err error
		if run, err = s.cfg.Store.GetRun(r.Context(), claims.RunID); err != nil {
			if !notFoundIf(w, err, "run", reasonRunNotFound) {
				writeServerError(w, r, "load run", err)
			}
			return
		}
	}
	if part > types.RecordingMaxParts {
		runID := claims.RunID
		s.recordAudit(r.Context(), s.auditEvent(&runID, types.ActorAgent, claims.SPIFFEID, "recording.upload",
			runID.String(), "failure", mustJSON(labelRecordingEvidence(run, map[string]any{"part": part, "reason": reasonRecordingPartLimit}))))
		writeErrorReason(w, http.StatusRequestEntityTooLarge, reasonRecordingPartLimit, "recording exceeds its part limit")
		return
	}

	if s.refuseDeclinedUpload(w, run) {
		runID := claims.RunID
		s.recordAudit(r.Context(), s.auditEvent(&runID, types.ActorAgent, claims.SPIFFEID, "recording.upload",
			runID.String(), "failure", mustJSON(labelRecordingEvidence(run, map[string]any{"part": part, "reason": reasonRecordingOff}))))
		return
	}

	saveErr := s.storeUpload(w, r, run, claims.RunID, part)
	cleared := false
	if saveErr == nil {
		cleared = s.clearDeclinedOnUpload(r.Context(), run)
		saveErr = s.uploadedRecordingOutput(r.Context(), claims.RunID)
	}

	// Audit BOTH outcomes, like every sibling recording lane: a full store or
	// an over-cap upload is exactly how a long session's provenance gets lost,
	// and a success-only trail renders that loss invisible.
	runIDUUID := claims.RunID
	outcome := "success"
	fields := map[string]any{}
	if part > 1 {
		fields["part"] = part
	}
	if cleared {
		fields["not_recorded_cleared"] = types.NotRecordedRunnerDeclined
	}
	if saveErr != nil {
		outcome = "failure"
		fields["error"] = saveErr.Error()
		if errors.Is(saveErr, recording.ErrErased) {
			fields["reason"] = reasonRecordingErased
		}
	}
	labelRecordingEvidence(run, fields)
	var data []byte
	if len(fields) > 0 {
		data = mustJSON(fields)
	}
	// Detached: a client that hung up (HTTP/1 cancels the request context) must not cost the outcome row.
	s.recordAudit(context.WithoutCancel(r.Context()), s.auditEvent(
		&runIDUUID,
		types.ActorAgent,
		claims.SPIFFEID,
		"recording.upload",
		claims.RunID.String(),
		outcome,
		data,
	))

	if errors.Is(saveErr, errMaskUncovered) {
		// The run was fenced or lost its manifest mid-upload: nothing past
		// that point was masked against a corpus proven whole.
		s.refuseAgent(w, r, claims, "recordings.upload")
		return
	}
	if saveErr != nil {
		// An over-cap upload surfaces as *http.MaxBytesError through the masker.
		var maxErr *http.MaxBytesError
		if errors.As(saveErr, &maxErr) {
			writeErrorReason(w, http.StatusRequestEntityTooLarge, reasonRecordingTooLarge, "recording exceeds size limit")
			return
		}
		if errors.Is(saveErr, recording.ErrErased) {
			writeErrorReason(w, http.StatusGone, reasonRecordingErased, "recordings for this run have been erased")
			return
		}
		writeServerError(w, r, "save recording", saveErr)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func buildMaskingBody(src io.Reader, reg *secretmask.Registry, runID uuid.UUID) io.Reader {
	return buildGuardedMaskingBody(src, reg, runID, nil)
}

// buildGuardedMaskingBody is buildMaskingBody whose reader also ends with
// errMaskUncovered when guard (non-nil) turns false: the run was fenced while
// its upload was in flight, and the rest of it is not read.
func buildGuardedMaskingBody(src io.Reader, reg *secretmask.Registry, runID uuid.UUID, guard func() bool) io.Reader {
	if reg == nil {
		return src
	}
	snap := reg.Snapshot(runID)
	if len(snap) == 0 && guard == nil {
		return src
	}
	// The upload body is asciicast JSON: asciinema (which wardyn-rec execs)
	// json-encodes each terminal-output chunk into an "o" event, so a secret containing any byte
	// JSON escapes (newline, quote, backslash, control char) never appears
	// verbatim in the body — and the Masker's exact-byte match would miss it.
	// Critically that includes a MULTI-LINE SSH private key, which broker.mint()
	// mask-registers (internal/broker/broker.go) precisely so PTY/asciicast
	// streams mask it. secretmask.JSONEscapedVariants adds each secret's
	// JSON-string-escaped rendering so those land masked on this path too (the
	// same expansion the audit maskingRecorder applies to ev.Data).
	// Residual (disclosed in THREAT-MODEL.md and secretmask.Mask): a secret SPLIT
	// across two asciicast events by wardyn-rec's PTY read boundaries is still not
	// caught — the `"],[t,"o","` event framing breaks the verbatim byte run,
	// which no per-value match closes.
	snap = secretmask.JSONEscapedVariants(snap)
	r := &recordingMaskReader{src: src, guard: guard, mask: secretmask.NewMasker(snap), limit: recording.MaxCastBytes}
	r.masker = secretmask.NewMaskingWriter(&r.output, r.mask)
	return r
}

type recordingMaskReader struct {
	src    io.Reader
	output bytes.Buffer
	mask   secretmask.Masker
	masker *secretmask.MaskingWriter
	chunk  [32 << 10]byte
	// held are the last bytes read, kept from the masker until the end says whether they are a log cut marker.
	held  []byte
	err   error
	guard func() bool // nil, or false once the run is no longer covered
	// limit is the most masked bytes a plain log may leave this reader. Masking can lengthen a body, so a log
	// within the upload cap can come out past the store's. Past limit the output is cut and ends with
	// maskCutMarker(limit) in place of the rest. The cut is of masked bytes after the masker has flushed, so it
	// cannot split a secret. A cast is never cut here, its lines would break: the store refuses it.
	limit int
	sent  int    // masked bytes handed to the caller
	head  []byte // the first bytes read, to tell a cast from a plain log
	cut   bool
	// plainLog is the recorder's own word that the body is a plain log, whatever its first bytes are.
	plainLog bool
}

// isCast says whether the body is a cast, which is never cut: by the recorder's word when it gave one,
// else by the cast header's first bytes.
func (r *recordingMaskReader) isCast() bool {
	return !r.plainLog && strings.HasPrefix(string(r.head), castHeadPrefix)
}

// maskCutMarker is the line a log cut at limit masked bytes ends with. It is not wardyn-rec's cut marker, which
// says the sender cut the log; this one says the server did. A log wardyn-rec already cut that then passes the
// limit ends with this marker alone: its own marker, and the original size it carried, are dropped with the rest.
func maskCutMarker(limit int) string {
	return fmt.Sprintf("\n[wardyn: log truncated: the masked log passed the %d byte limit; the rest was dropped]\n", limit)
}

const castHeadPrefix = `{"version"`

// held back from the caller until the end or the cap is known: the room the cut marker needs.
func (r *recordingMaskReader) holdBack() int {
	if r.err != nil || r.cut || r.limit == 0 || r.isCast() {
		return 0
	}
	return len(maskCutMarker(r.limit))
}

// capOutput cuts a plain log's masked output at the limit: whatever was not yet handed on, up to the room the
// marker needs, then the marker. The source is not read again.
func (r *recordingMaskReader) capOutput() {
	if r.limit == 0 || r.cut || r.sent+r.output.Len() <= r.limit || r.isCast() {
		return
	}
	marker := maskCutMarker(r.limit)
	r.output.Truncate(r.limit - len(marker) - r.sent)
	r.output.WriteString(marker)
	r.cut, r.held = true, nil
	if r.err == nil {
		r.err = io.EOF
	}
}

// finish masks the held bytes and flushes the masker. A body that ends with wardyn-rec's log cut marker
// was cut just before it, so a secret the cut split is masked as far as it arrived.
func (r *recordingMaskReader) finish(eof bool) error {
	held := r.held
	r.held = nil
	if i := recording.LogCutAt(held); eof && i >= 0 {
		_, err := r.masker.Write(held[:i])
		err = cmp.Or(err, r.masker.CloseCut())
		r.output.Write(r.mask.Mask(held[i:]))
		return err
	}
	_, err := r.masker.Write(held)
	return cmp.Or(err, r.masker.Close())
}

func (r *recordingMaskReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.output.Len() <= r.holdBack() && r.err == nil {
		if r.guard != nil && !r.guard() {
			r.err = errMaskUncovered
			return 0, r.err
		}
		n, readErr := r.src.Read(r.chunk[:])
		r.held = append(r.held, r.chunk[:n]...)
		if room := len(castHeadPrefix) - len(r.head); room > 0 {
			r.head = append(r.head, r.chunk[:min(n, room)]...)
		}
		var maskErr error
		if over := len(r.held) - recording.LogCutMarkerMax; over > 0 {
			_, maskErr = r.masker.Write(r.held[:over])
			r.held = append(r.held[:0], r.held[over:]...)
		}
		r.err = cmp.Or(maskErr, readErr)
		r.capOutput()
		if r.err != nil && !r.cut {
			// Flush the retained masked tail before exposing EOF or the source
			// error, including MaxBytesError. Neither may discard buffered bytes.
			closeErr := r.finish(r.err == io.EOF)
			if r.err == io.EOF && closeErr != nil {
				r.err = closeErr
			}
			r.capOutput()
		}
		if n == 0 && r.err == nil {
			return 0, nil
		}
	}
	n, _ := r.output.Read(p[:min(len(p), r.output.Len()-r.holdBack())])
	r.sent += n
	if r.output.Len() > 0 {
		return n, nil
	}
	return n, r.err
}
