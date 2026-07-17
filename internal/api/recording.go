// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

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

	// Finding 3 (DoS): cap the upload so an authenticated agent cannot exhaust
	// disk with an unbounded cast. MaxBytesReader returns an *http.MaxBytesError
	// once the cap is exceeded; the masking copy goroutine propagates it through
	// the pipe so SaveCast fails, and we map it to 413 below.
	limited := http.MaxBytesReader(w, r.Body, maxRecordingUploadBytes)

	// PRIMARY masking point: pipe the upload body through a MaskingWriter so
	// verbatim secret values are replaced with "<secret-hidden>" before the bytes
	// reach the RecordingStore. A nil MaskRegistry is a safe no-op (pass-through).
	//
	// HONEST RESIDUAL: only verbatim byte-identical occurrences are masked.
	// base64-encoded, hex-encoded, or model-narrated representations of secrets
	// are NOT caught. This is intentional — masking catches the most likely
	// accidental leakage vector (token printed to stdout/asciicast).
	//
	// Implementation: io.Pipe bridges the MaskingWriter (io.Writer) to SaveCast
	// (io.Reader). The copy goroutine reads from the (size-limited) body, writes
	// through the masker into pw, then closes pw so SaveCast's read returns
	// io.EOF cleanly. cleanup() MUST run on every path (Finding 2): if SaveCast
	// aborts the read side early it would otherwise leave the copy goroutine
	// blocked forever on pw.Write (a goroutine + body leak). cleanup closes the
	// read end (unblocking the writer) and awaits the goroutine.
	body, cleanup := buildMaskingBody(limited, s.cfg.MaskRegistry, claims.RunID)
	defer cleanup()

	if err := s.cfg.RecordingStore.SaveCast(r.Context(), claims.RunID.String(), body); err != nil {
		// An over-cap upload surfaces as *http.MaxBytesError through the pipe.
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "recording exceeds size limit")
			return
		}
		writeError(w, http.StatusInternalServerError, "save recording: "+err.Error())
		return
	}

	runIDUUID := claims.RunID
	s.recordAudit(r.Context(), s.auditEvent(
		&runIDUUID,
		types.ActorAgent,
		claims.SPIFFEID,
		"recording.upload",
		claims.RunID.String(),
		"success",
		nil,
	))

	w.WriteHeader(http.StatusNoContent)
}

// buildMaskingBody returns an io.Reader that transparently masks secrets from
// src before its bytes are consumed by the caller, plus a cleanup func the
// caller MUST always invoke (defer) once it is done reading.
//
// When reg is nil or has no secrets for runID, src is returned unchanged and
// cleanup is a no-op (nil-safe, no goroutine cost).
//
// Otherwise a copy goroutine bridges a MaskingWriter to an io.Pipe. Finding 2:
// if the consumer (SaveCast) aborts the read side early — e.g. a store/path
// error after a partial read — the copy goroutine would block forever on
// pw.Write (no reader) and leak the goroutine AND the underlying request body.
// cleanup closes the read end (CloseWithError makes any pending/future pw.Write
// return immediately) and then awaits the goroutine, guaranteeing it has
// exited. Calling cleanup after a normal full read is safe and cheap (the
// goroutine has already finished and pr.Close is idempotent).
func buildMaskingBody(src io.Reader, reg *secretmask.Registry, runID uuid.UUID) (io.Reader, func()) {
	if reg == nil {
		return src, func() {}
	}
	snap := reg.Snapshot(runID)
	if len(snap) == 0 {
		return src, func() {}
	}
	m := secretmask.NewMasker(snap)

	// Bridge MaskingWriter (io.Writer) to SaveCast (io.Reader) via io.Pipe.
	pr, pw := io.Pipe()
	mw := secretmask.NewMaskingWriter(pw, m)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, cpErr := io.Copy(mw, src)
		// Always close the MaskingWriter to flush the retained tail. mw.Close()
		// flushes only — it does NOT close pw (secretmask ownership invariant), so
		// the CloseWithError below is the single authority on how the read side
		// terminates and cpErr always reaches SaveCast. If mw ever starts closing
		// its downstream again, io.Pipe's once-only error store would keep the EOF
		// stamped here and discard cpErr — the 413 cap would go dead.
		closeErr := mw.Close()
		if cpErr != nil {
			_ = pw.CloseWithError(cpErr)
		} else if closeErr != nil {
			_ = pw.CloseWithError(closeErr)
		} else {
			_ = pw.Close()
		}
	}()
	cleanup := func() {
		// Closing the read end unblocks any pw.Write the goroutine is parked on
		// (it returns io.ErrClosedPipe), so the goroutine can run to completion.
		_ = pr.CloseWithError(io.ErrClosedPipe)
		<-done
	}
	return pr, cleanup
}
