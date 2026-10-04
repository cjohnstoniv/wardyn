// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /runs/{id}/output (#1232) serves the end of a non-interactive run's
// combined stdout/stderr. wardynd keeps it in memory while the run lives, apart
// from the recording store, so a deployment with WARDYN_RECORDING_STORE=off can
// still read what its headless runs printed. An interactive run is refused: its
// terminal is the recording's to keep, never this route's.
//
// The tail is masked as it is written, the same way a live attach's recording
// is (liveMaskWriter), so a value the masking registry holds when it is
// printed never lands in it. A secret the registry does not hold yet, printed
// by the command, is kept like any log line (docs/OPERATIONS.md).
//
// With persistence on (WARDYN_RUN_OUTPUT_PERSIST, the default) the final tail
// is written once to run_outputs by the finalisation contract
// (run_output_final.go) and the memory copy is released; with it off the tail
// lives only in memory, for WARDYN_EXEC_OUTPUT_TAIL_TTL after its last output.

const (
	// defaultRunOutputTailBytes is WARDYN_RUN_OUTPUT_TAIL_BYTES's default: it
	// bounds each run's tail and what ?tail= may ask for.
	defaultRunOutputTailBytes = 64 << 10
	// defaultExecOutputTailTTL is WARDYN_EXEC_OUTPUT_TAIL_TTL's default.
	defaultExecOutputTailTTL = 24 * time.Hour
)

// outputRing keeps the last max bytes written to it. Not locked on
// its own: every access holds its execOutputTail's liveMaskWriter mutex.
type outputRing struct {
	max       int
	buf       []byte
	truncated bool // bytes were dropped off the front
	expired   bool // the TTL passed: buf is released and later writes are dropped
	last      time.Time
	now       func() time.Time
}

// Write never fails: the runner drains the exec through it.
func (r *outputRing) Write(p []byte) (int, error) {
	n := len(p)
	if r.expired {
		return n, nil
	}
	if r.buf == nil {
		r.buf = make([]byte, 0, r.max)
	}
	if len(p) > r.max {
		p = p[len(p)-r.max:]
		r.truncated = true
	}
	if drop := len(r.buf) + len(p) - r.max; drop > 0 {
		r.buf = append(r.buf[:0], r.buf[drop:]...)
		r.truncated = true
	}
	r.buf = append(r.buf, p...)
	r.last = r.now()
	return n, nil
}

// execOutputTail is one run's tail: the masking writer the runner writes to,
// in front of the ring it fills, plus the finalisation state
// (run_output_final.go): the open drains, the once-guard and the erasure fence.
type execOutputTail struct {
	mw   *liveMaskWriter
	ring outputRing

	dmu       sync.Mutex    // guards drains, drainErr and drainWake
	drains    int           // copies into this tail that have begun and not ended
	drainErr  bool          // a copy ended in an error
	drainWake chan struct{} // closed and replaced whenever drains changes

	fmu      sync.Mutex    // guards started, committed, late, fenced, recovered and gapReason
	started  bool          // a finisher has taken the once-guard
	done     chan struct{} // closed when that finisher returns
	finished bool          // the tail is sealed: barrier closed, holdback flushed
	// committed: the final row is in Postgres, so a byte dropped now marks it
	// incomplete rather than reaching the row as it is written.
	committed bool
	late      bool // a byte was dropped after the seal
	fenced    bool // the run's output was erased: this tail is zeroed and dropped
	// recovered: the bytes were read back from the substrate by a process that
	// did not capture them live (run_output_recover.go). gapReason, when set,
	// says the recovery found nothing it may keep, so the finisher writes a
	// capture gap instead of this tail.
	recovered bool
	gapReason string
}

func newExecOutputTail(max int, now func() time.Time) *execOutputTail {
	return &execOutputTail{
		ring:      outputRing{max: max, last: now(), now: now},
		drainWake: make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// execOutputTails holds every kept tail by run id. The zero value is ready.
//
// With persistence off an entry is pruned lazily, on the next open or read: TTL
// after its last output its buffer is released and it is marked expired, so a
// read can say so; after a second TTL the marker goes too. Memory is therefore
// bounded by the runs that printed within the last two TTLs. With persistence
// on a tail is released when its final row commits (or is erased), never by
// the TTL, so memory is bounded by the live and kept runs.
type execOutputTails struct {
	mu sync.Mutex
	m  map[uuid.UUID]*execOutputTail
}

func (t *execOutputTails) pruneLocked(now time.Time, ttl time.Duration) {
	for id, e := range t.m {
		e.mw.mu.Lock()
		idle := now.Sub(e.ring.last)
		if idle >= ttl {
			e.ring.expired, e.ring.buf, e.mw.tail = true, nil, nil
		}
		e.mw.mu.Unlock()
		if idle >= 2*ttl {
			delete(t.m, id)
		}
	}
}

// runOutputStore is the store persisting run output, or nil when persistence is
// off (WARDYN_RUN_OUTPUT_PERSIST=off, or a store that keeps none).
func (s *Server) runOutputStore() store.RunOutputStore {
	if s.cfg.RunOutputPersistOff {
		return nil
	}
	st, _ := s.cfg.Store.(store.RunOutputStore)
	return st
}

// openExecOutput starts runID's tail and returns the writer dispatch hands the
// runner (runner.SandboxSpec.ExecOutput), or nil when this run keeps none.
func (s *Server) openExecOutput(run types.AgentRun, interactive bool) io.Writer {
	if s.cfg.ExecOutputTailOff || interactive || runIsUnrecordable(run) {
		return nil
	}
	runID := run.ID
	// Door 3 of five (mask_manifest.go): the relay keeps nothing for a run whose
	// masking corpus this server cannot prove whole, and its writer drops a
	// chunk the moment that stops being true.
	if !s.maskCovered(context.Background(), runID) {
		return nil
	}
	// The pending row is the durable record that a capture is owed. A failed
	// insert is logged and does not fail the run: the final UPSERT still writes
	// the row. A tombstone means the run's output was erased: keep nothing.
	if st := s.runOutputStore(); st != nil {
		if err := st.InsertPendingRunOutput(context.Background(), runID); errors.Is(err, store.ErrRunOutputErased) {
			s.fenceRunOutput(runID)
			return nil
		} else if err != nil {
			slog.Warn("wardynd: could not record that a run's output capture is owed", slog.String("run_id", runID.String()), slog.Any("err", err))
		}
	}
	e := newExecOutputTail(s.cfg.RunOutputTailBytes, s.cfg.Now)
	e.mw = &liveMaskWriter{reg: s.cfg.MaskRegistry, runID: runID, dst: &e.ring, guard: s.maskGuard(runID), capture: outputCapture{onLate: func() { s.lateRunOutput(runID, e) }}}
	t := &s.execOutputs
	t.mu.Lock()
	defer t.mu.Unlock()
	s.pruneExecOutputsLocked()
	if t.m == nil {
		t.m = map[uuid.UUID]*execOutputTail{}
	}
	t.m[runID] = e
	return &tailWriter{mw: e.mw, t: e}
}

// releasedHoldback is what a masker's withheld bytes become once no more output
// can complete them: a held-back run of secretmask.MinLen bytes or more is a
// secret cut short and is hidden (masking it against itself yields the
// placeholder); a shorter one is ordinary output that shares a few leading
// bytes with a secret. The read of an unfinished tail and the finisher's flush
// both use it.
func releasedHoldback(held []byte) []byte {
	if len(held) == 0 {
		return nil
	}
	return secretmask.NewMasker([][]byte{held}).Mask(held)
}

// execOutputView is one tail as a read sees it.
type execOutputView struct {
	out       []byte
	truncated bool
	complete  bool // the capture is final: the barrier closed and the holdback is in out
	expired   bool // the TTL passed
	uncovered bool // the writer lost the run's complete masking manifest at some point
}

// readExecOutput returns at most limit bytes from the end of runID's tail.
// withHold adds the bytes the masker is still holding back as a possible
// secret prefix to an unfinished tail, for a run that has ended and so gets no
// more output. kept is false when no tail is held.
func (s *Server) readExecOutput(runID uuid.UUID, limit int, withHold bool) (v execOutputView, kept bool) {
	t := &s.execOutputs
	t.mu.Lock()
	s.pruneExecOutputsLocked()
	e := t.m[runID]
	t.mu.Unlock()
	if e == nil {
		return execOutputView{}, false
	}
	e.fmu.Lock()
	v.complete = e.finished
	e.fmu.Unlock()
	e.mw.mu.Lock()
	defer e.mw.mu.Unlock()
	if e.ring.expired {
		return execOutputView{expired: true}, true
	}
	v.uncovered = e.mw.capture.uncovered
	v.out = append([]byte(nil), e.ring.buf...)
	if withHold && !v.complete {
		v.out = append(v.out, releasedHoldback(e.mw.tail)...)
	}
	v.truncated = e.ring.truncated
	if len(v.out) > limit {
		v.out, v.truncated = v.out[len(v.out)-limit:], true
	}
	return v, true
}

// pruneExecOutputsLocked applies the TTL, in the memory-only mode only; the
// caller holds the map's lock.
func (s *Server) pruneExecOutputsLocked() {
	if s.runOutputStore() != nil {
		return
	}
	s.execOutputs.pruneLocked(s.cfg.Now(), s.cfg.ExecOutputTailTTL)
}

// runOutputResponse is GET /runs/{id}/output's body; client.RunOutput mirrors it.
type runOutputResponse struct {
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"` // output does not start at the run's first byte
	// Complete is true only for a final capture: a stored final row, or a
	// memory tail whose drain barrier closed and whose holdback was flushed. A
	// run that has finished is not yet complete until then.
	Complete   bool       `json:"complete"`
	Source     string     `json:"source"`                // "stdout"
	Incomplete bool       `json:"incomplete"`            // bytes may be missing: a drain timed out or failed, or a chunk was dropped
	CaptureGap bool       `json:"capture_gap"`           // the output could not be captured (no tail held, nothing to recover)
	MaskScope  string     `json:"mask_scope,omitempty"`  // "run": masked against the run's complete manifest throughout; "globals_only": not
	CapturedAt *time.Time `json:"captured_at,omitempty"` // when the final row was written; omitted while live
}

// handleRunOutput serves GET /api/v1/runs/{id}/output?tail=N — owner or admin,
// with GET /runs/{id}'s 404 for anyone else. The one other 404 is
// run_output_erased, and it cannot leak existence: only a caller who may see
// the run gets past getRunAuthorized, and the reason tells them what happened.
//
// After getRunAuthorized the answer is, in order: erased (404); output off
// (409); a final row, served on any replica with no manifest and no lease
// (200); a live read of an uncovered run (503, ha-l2.0's refusal); a live
// tail in this process (200); a pending row with no local tail (409, read again shortly);
// an interactive run (409); a run that ended longer ago than the retention
// window (410); otherwise not kept (409).
func (s *Server) handleRunOutput(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	limit := s.cfg.RunOutputTailBytes
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeErrorReason(w, http.StatusBadRequest, reasonRunOutputTailInvalid, "tail must be a positive number of bytes")
			return
		}
		limit = min(n, s.cfg.RunOutputTailBytes)
	}
	row, found, err := s.readStoredRunOutput(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrRunOutputErased):
		s.fenceRunOutput(id)
		writeErrorReason(w, http.StatusNotFound, reasonRunOutputErased, "this run's output was erased")
		return
	case err != nil:
		writeServerError(w, r, "read run output", err)
		return
	}
	if s.cfg.ExecOutputTailOff {
		writeErrorReason(w, http.StatusConflict, reasonRunOutputOff,
			"this deployment keeps no run output (WARDYN_EXEC_OUTPUT_TAIL=off)")
		return
	}
	// A final row was masked before it was stored, so it needs no manifest here:
	// it is what makes a terminal run's output readable after every replica
	// restarted.
	if found && row.CapturedAt != nil {
		out, truncated := row.Output, row.Truncated
		if len(out) > limit {
			out, truncated = out[len(out)-limit:], true
		}
		writeJSON(w, http.StatusOK, runOutputResponse{
			Output: string(out), Truncated: truncated, Complete: true, Source: row.Source, Incomplete: row.Incomplete,
			CaptureGap: row.CaptureGap, MaskScope: row.MaskScope, CapturedAt: row.CapturedAt,
		})
		return
	}
	// Door 5 of five (mask_manifest.go): the tail in this process was masked
	// against a registry this server must be able to prove whole. It guards the
	// live read: a run still running, or a tail this process holds. A terminal
	// run with neither has only a stored row or nothing, and answers below.
	if (!run.State.IsTerminal() || s.tailFor(id) != nil) && s.refuseUncovered(w, r, id, "runs.output") {
		return
	}
	if v, kept := s.readExecOutput(id, limit, run.State.IsTerminal()); kept {
		if v.expired {
			writeErrorReason(w, http.StatusGone, reasonRunOutputExpired,
				"this run's output has expired: it is kept for WARDYN_EXEC_OUTPUT_TAIL_TTL after the run's last output")
			return
		}
		writeJSON(w, http.StatusOK, runOutputResponse{
			Output: string(v.out), Truncated: v.truncated, Complete: v.complete, Source: "stdout",
			MaskScope: s.liveMaskScope(v.uncovered),
		})
		return
	}
	switch {
	case found:
		writeErrorReason(w, http.StatusConflict, reasonRunOutputNotKept,
			"this run's output is still being captured: read it again shortly")
	case run.Interactive:
		writeErrorReason(w, http.StatusConflict, reasonRunOutputInteractive,
			"an interactive run keeps no output here: its terminal is the recording's to keep")
	case s.runOutputExpiredByRetention(run):
		writeErrorReason(w, http.StatusGone, reasonRunOutputExpired,
			"this run's output has expired: it is kept for WARDYN_RUN_OUTPUT_RETENTION_DAYS after the run ended")
	default:
		writeErrorReason(w, http.StatusConflict, reasonRunOutputNotKept, "no output is kept for this run")
	}
}

// readStoredRunOutput reads runID's row, checking its erasure tombstone in the
// same transaction. A deployment with no run-output store has none.
func (s *Server) readStoredRunOutput(ctx context.Context, runID uuid.UUID) (store.RunOutput, bool, error) {
	st, _ := s.cfg.Store.(store.RunOutputStore)
	if st == nil {
		return store.RunOutput{}, false, nil
	}
	return st.GetRunOutput(ctx, runID)
}

// runOutputExpiredByRetention reports whether run ended longer ago than
// WARDYN_RUN_OUTPUT_RETENTION_DAYS, so a missing row is a deleted one.
func (s *Server) runOutputExpiredByRetention(run types.AgentRun) bool {
	return s.cfg.RunOutputRetention > 0 && run.EndedAt != nil && s.cfg.Now().Sub(*run.EndedAt) > s.cfg.RunOutputRetention
}
