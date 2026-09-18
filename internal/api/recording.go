// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"cmp"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordingAuthorizer is the owner-or-admin authorization callback wired into
// recording.Handler (item 4, server.go's Mount call): a run's recording is
// exactly as sensitive as its live PTY attach (it may show whatever the
// agent's injected credentials left on screen), so it gets the SAME
// ownership gate as getRunAuthorized — an admin, or the run's own creator,
// may read it. runIDPrefix is the cast key's run-id portion (Handler has
// already enforced it equals the outer mount {id} before this ever runs); a
// prefix that doesn't parse as a run id, or names no real run, is denied
// (fails closed) rather than erroring — recording.Handler turns any false
// return into the SAME 404 "recording not found" a truly-missing cast gets,
// so this never needs to shape its own response.
func (s *Server) recordingAuthorizer(r *http.Request, runIDPrefix string) bool {
	id, err := uuid.Parse(runIDPrefix)
	if err != nil {
		return false
	}
	// DELIBERATELY isOperator (three-tier doctrine, internal/auth/oidc's
	// RoleSecurityAdmin): a recording is a PRIVACY surface — a replay of
	// someone's terminal with their agent's injected credentials on screen.
	// Governing policy does not include watching people work, and this is the
	// one place ownsRunOrAdmin's incident-response widening must NOT reach.
	if s.isOperator(r.Context()) {
		return true
	}
	run, err := s.cfg.Store.GetRun(r.Context(), id)
	if err != nil {
		return false
	}
	if run.CreatedBy == principalFromRequest(r) {
		return true
	}
	// M1: audited only once the run is confirmed to genuinely exist — a
	// malformed prefix or an unknown run (both branches above) stays silent;
	// only a POSITIVELY identified foreign run reaches this audit. The 404
	// recording.Handler writes on a false return is unaffected either way.
	s.recordAudit(r.Context(), s.auditEvent(&id, actorTypeFromRequest(r), principalFromRequest(r),
		"authz.denied", id.String(), "denied", mustJSON(map[string]any{"reason": "not_owner"})))
	return false
}

// maxRecordingUploadBytes caps a single recording PUT (Finding 3: DoS / disk
// exhaustion). An authenticated in-sandbox agent could otherwise stream an
// unbounded cast and fill the control plane's disk. 64 MiB comfortably holds a
// long PTY session's asciicast while bounding a single hostile upload. Bytes
// beyond the cap cause http.MaxBytesReader to error, which we surface as 413.
const maxRecordingUploadBytes = 64 << 20 // 64 MiB

// handleUploadRecording accepts a PUT /api/v1/internal/recordings/{runID} from
// wardyn-rec running inside the agent container. The caller must hold a valid
// run token (enforced by internalAuth). The run ID in the path must match the
// sub claim of the token to prevent cross-run pollution.
//
// The request body is the raw asciicast stream. Content-Type is not enforced
// so the fallback .log uploads also work.
func (s *Server) handleUploadRecording(w http.ResponseWriter, r *http.Request) {
	// Cross-run guard: the caller must hold the run's OWN token — prevent a
	// token from run A uploading a recording under run B.
	claims, ok := claimsForRunUpload(w, r)
	if !ok {
		return
	}

	if s.cfg.RecordingStore == nil {
		writeError(w, http.StatusNotImplemented, "recording store not configured")
		return
	}

	// Cap the upload before masking; the reader preserves MaxBytesError so an
	// over-cap upload remains a 413 rather than a successful truncated cast.
	limited := http.MaxBytesReader(w, r.Body, maxRecordingUploadBytes)

	// PRIMARY masking point: read the upload body through a MaskingWriter so
	// verbatim secret values are replaced with "<secret-hidden>" before the bytes
	// reach the RecordingStore. A nil MaskRegistry is a safe no-op (pass-through).
	//
	// HONEST RESIDUAL: only verbatim byte-identical occurrences are masked.
	// base64-encoded, hex-encoded, or model-narrated representations of secrets
	// are NOT caught. This is intentional — masking catches the most likely
	// accidental leakage vector (token printed to stdout/asciicast).
	//
	// Read only on the store's demand: an early storage failure must not leave
	// an independent goroutine blocked reading the request body.
	body := buildMaskingBody(limited, s.cfg.MaskRegistry, claims.RunID)

	saveErr := s.cfg.RecordingStore.SaveCast(r.Context(), claims.RunID.String(), body)

	// Audit BOTH outcomes, like every sibling recording lane: a full store or
	// an over-cap upload is exactly how a long session's provenance gets lost,
	// and a success-only trail renders that loss invisible.
	runIDUUID := claims.RunID
	outcome := "success"
	var data []byte
	if saveErr != nil {
		outcome = "failure"
		data = mustJSON(map[string]any{"error": saveErr.Error()})
	}
	s.recordAudit(r.Context(), s.auditEvent(
		&runIDUUID,
		types.ActorAgent,
		claims.SPIFFEID,
		"recording.upload",
		claims.RunID.String(),
		outcome,
		data,
	))

	if saveErr != nil {
		// An over-cap upload surfaces as *http.MaxBytesError through the masker.
		var maxErr *http.MaxBytesError
		if errors.As(saveErr, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "recording exceeds size limit")
			return
		}
		writeError(w, http.StatusInternalServerError, "save recording: "+saveErr.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func buildMaskingBody(src io.Reader, reg *secretmask.Registry, runID uuid.UUID) io.Reader {
	if reg == nil {
		return src
	}
	snap := reg.Snapshot(runID)
	if len(snap) == 0 {
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
	// same expansion the audit maskingRecorder now applies to ev.Data — D31).
	// RESIDUAL (disclosed in THREAT-MODEL.md and secretmask.Mask): a secret SPLIT
	// across two asciicast events by wardyn-rec's PTY read boundaries is still not
	// caught — the `"],[t,"o","` event framing breaks the verbatim byte run,
	// which no per-value match closes.
	snap = secretmask.JSONEscapedVariants(snap)
	r := &recordingMaskReader{src: src}
	r.masker = secretmask.NewMaskingWriter(&r.output, secretmask.NewMasker(snap))
	return r
}

type recordingMaskReader struct {
	src    io.Reader
	output bytes.Buffer
	masker *secretmask.MaskingWriter
	chunk  [32 << 10]byte
	err    error
}

func (r *recordingMaskReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.output.Len() == 0 && r.err == nil {
		n, readErr := r.src.Read(r.chunk[:])
		_, maskErr := r.masker.Write(r.chunk[:n])
		r.err = cmp.Or(maskErr, readErr)
		if r.err != nil {
			// Flush the retained masked tail before exposing EOF or the source
			// error, including MaxBytesError. Neither may discard buffered bytes.
			closeErr := r.masker.Close()
			if r.err == io.EOF && closeErr != nil {
				r.err = closeErr
			}
		}
		if n == 0 && r.err == nil {
			return 0, nil
		}
	}
	n, _ := r.output.Read(p)
	if r.output.Len() > 0 {
		return n, nil
	}
	return n, r.err
}
