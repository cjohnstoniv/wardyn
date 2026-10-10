// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"io"
	"sync"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runIsUnrecordable reports whether a run's terminal must never be persisted as
// an asciicast, because the run exists precisely to display a live credential.
//
// The managed-harness login run shells out to `claude setup-token`, which PRINTS
// a ~1-year Anthropic OAuth token to the PTY. Recording it would write that
// credential verbatim into a replayable artifact that long outlives the run —
// and no masking can prevent it (the token is unknown to wardynd until the
// operator pastes it back, by which point the cast already holds it). Dropping
// the cast costs no provenance: harness.login.start and session.attach still
// record who attached, when, and why.
func runIsUnrecordable(run types.AgentRun) bool {
	return run.Task == harnessLoginTask
}

// newSessionRecorder builds the interactive-session recording pipeline and
// returns (tee, finish):
//
//   - tee is the io.Writer the attach pump feeds PTY OUTPUT into. It is the
//     front of: maskPipe (which batches the output so the pump never waits on
//     the masker's registry read) -> liveMaskWriter (re-snapshotting secret
//     masking with a retained cross-write tail) -> CastWriter (asciicast v2) ->
//     an in-memory buffer. When
//     no RecordingStore is configured tee is nil and finish is a no-op, so attach
//     works unchanged in headless/no-store mode.
//   - finish flushes the masker tail and persists the buffered asciicast to the
//     RecordingStore under a per-run+session key (so it never clobbers the batch
//     run's cast or a concurrent attach), then emits a session.recording.write audit
//     event. It is best-effort: a persist failure is audited as a failure but
//     never fails the detach.
//
// Security: the recording is masked against the MaskRegistry (invariant 1: no
// verbatim secret leakage into the recording). Unlike the agent UPLOAD path
// (which snapshots once at run end, after every mint is registered), an attach
// is live, so the masker RE-SNAPSHOTS the registry on each write to catch a
// credential minted mid-session. When MaskRegistry is nil it is a safe
// pass-through (documented residual unchanged).
//
// Not-recorded runs: a run whose terminal exists to PRINT a credential is never
// recorded at all — see runIsUnrecordable. Masking cannot protect those: the
// registry only masks values it already holds, and such a token is unknown to
// wardynd until the operator pastes it back, which is strictly AFTER the bytes
// would have landed in the cast. The gate lives here, not at the call site, so
// a future second caller cannot miss it.
// maxSessionCastBytes bounds the asciicast a LIVE attach buffers in the daemon's
// heap. The cast is held in memory for the whole session and written once at
// close, so without a bound a single long, chatty terminal grows unopposed
// inside a control plane deployed with a 512Mi limit — and, worse, the store
// then REJECTED the result wholesale at its own 64 MiB cap
// (internal/recording/pgstore.go), so the session's entire evidence was thrown
// away at exactly the moment it was meant to be persisted.
//
// Well under the store cap, so a recorded session is never lost to that reject.
//
// ponytail: past the cap the recording keeps its HEAD and drops the rest —
// smallest thing that keeps a valid, replayable artifact plus an honest
// truncated:true in the session.recording.write audit. A ring buffer that keeps the
// TAIL instead is the upgrade path if operators ask for the end of long
// sessions; a streaming sink is the one after that.
const maxSessionCastBytes = 8 << 20

// capBuffer is the bounded sink under a session CastWriter. CastWriter emits one
// complete JSON line per Write, so refusing a write WHOLE (never partially)
// leaves the buffer line-aligned and the cast well-formed. A dropped write is
// reported as accepted: the recording is best-effort provenance and must never
// break the live terminal.
//
// Not independently locked: every write reaches it through liveMaskWriter.Write
// and every read through finishRecording, both under that writer's mutex.
type capBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if b.truncated || b.buf.Len()+len(p) > maxSessionCastBytes {
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (s *Server) newSessionRecorder(run types.AgentRun, sessionID string, opts runner.AttachOptions) (io.Writer, func(ctx context.Context, principalType types.ActorType, principal string)) {
	noop := func(context.Context, types.ActorType, string) {}
	if s.cfg.RecordingStore == nil || runIsUnrecordable(run) {
		return nil, noop
	}
	runID := run.ID

	buf := &capBuffer{}
	cast := recording.NewCastWriter(buf, int(opts.Cols), int(opts.Rows), s.cfg.Now().UTC())

	// Mask the OUTPUT before it lands in the cast, RE-SNAPSHOTTING the registry on
	// every write so a credential minted MID-SESSION (after attach start) is masked
	// too, AND retaining a cross-write tail (the trailing bytes that form an
	// in-progress secret) so a secret whose verbatim bytes straddle two adjacent
	// ~32 KiB PTY chunks is reassembled and masked (FIX #12) — the same
	// split-secret defense the brokered-upload path's MaskingWriter provides. The
	// per-run secret set is tiny so per-write masking is cheap. A nil registry /
	// empty snapshot is a pass-through (the asciicast is still well-formed).
	mw := &liveMaskWriter{reg: s.cfg.MaskRegistry, runID: runID, dst: cast, guard: s.maskGuard(runID)}
	tee := newMaskPipe(mw)

	finish := func(ctx context.Context, principalType types.ActorType, principal string) {
		// What the pump teed before it ended is masked into the cast first.
		tee.flush()
		// Take the masker lock across (a) flushing the retained tail
		// into the cast and (b) reading the recording buffer, so a secret sitting in
		// the tail at session end is still masked (not dropped or leaked) and the
		// buffer is never read while the attach Read pump is mid-write. mw.mu is the
		// single mutex serialising the whole mask -> cast -> buf advance, so the read
		// below cannot race a concurrent Write from the pump goroutine.
		mw.mu.Lock()
		mw.flushLocked()
		had := cast.HadOutput()
		// Copy under the lock; the pump may keep appending after we release it.
		snap := append([]byte(nil), buf.buf.Bytes()...)
		truncated := buf.truncated
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
		if truncated {
			// Say so in the trail rather than let a short cast pass for a whole one.
			data["truncated"] = true
			data["limit_bytes"] = maxSessionCastBytes
		}
		if err != nil {
			outcome = "failure"
			data["error"] = err.Error()
		}
		s.recordStreamAudit(ctx, run.SandboxRef, s.auditEvent(&runID, principalType, principal, "session.recording.write",
			key, outcome, mustJSON(data)))
	}

	return tee, finish
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
	// guard, when non-nil, says whether the run's masking corpus is still
	// proven whole (mask_manifest.go). While it is false a chunk is dropped
	// instead of masked against a corpus that may be incomplete: the writer
	// never forwards bytes it cannot vouch for.
	guard func() bool
	// capture is the run-output capture's seal and marks (run_output_final.go);
	// the zero value, which an attach's recording keeps, seals nothing.
	capture outputCapture
}

func (w *liveMaskWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	n, late, err := w.writeLocked(p)
	w.mu.Unlock()
	if late != nil {
		late()
	}
	return n, err
}

// writeLocked is Write under w.mu. late is the seal's one-time notice, for the
// caller to call once the lock is released.
func (w *liveMaskWriter) writeLocked(p []byte) (n int, late func(), err error) {
	if late, sealed := w.capture.dropSealed(); sealed {
		return len(p), late, nil
	}
	// The read goes first: a read that applies an erasure's tombstones drops
	// values, and the guard must answer for the corpus the chunk is masked
	// against, not one from before the drop.
	if stale, err := w.replaceIfStale(len(p)); stale {
		return len(p), nil, err
	}
	if w.guard != nil && !w.guard() {
		w.tail = nil
		w.capture.dropped, w.capture.uncovered = true, true
		return len(p), nil, nil
	}

	// One CACHED masker per registry generation, not NewMasker(Snapshot(...)) per
	// chunk: the pair cloned every secret twice and sorted the whole set on every
	// PTY write, which is why a 32 KiB chunk went from 29.8us at one secret to
	// 1.51ms at 1024. Masker.Secrets is the same corpus the snapshot was,
	// longest-first, so pendingTailLen below reads it off the cached masker
	// instead of taking a second snapshot of the unchanged set.
	masker := w.reg.Masker(w.runID)
	snap := masker.Secrets()
	// Prepend the withheld tail so a secret straddling the previous/this write is
	// reassembled before masking. Re-masking already-masked tail bytes is a no-op
	// (the placeholder contains no secret).
	buf := append(w.tail, p...) //nolint:gocritic // intentional tail+chunk join
	masked := masker.Mask(buf)

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
			return 0, nil, err
		}
	}
	return len(p), nil, nil
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
	masked := w.reg.Masker(w.runID).Mask(w.tail)
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
