// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /runs/{id}/output (#1232) serves the end of a non-interactive run's
// combined stdout/stderr. wardynd keeps it in memory, apart from the recording
// store, so a deployment with WARDYN_RECORDING_STORE=off can still read what
// its headless runs printed. An interactive run is refused: its terminal is
// the recording's to keep, never this route's.
//
// The tail is masked as it is written, the same way a live attach's recording
// is (liveMaskWriter), so a value the masking registry holds when it is
// printed never lands in it. A secret the registry does not hold yet, printed
// by the command, is kept like any log line (docs/OPERATIONS.md).
//
// ponytail: memory only, so a wardynd restart loses every tail; persist it
// only if someone needs one across a restart.

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
// in front of the ring it fills.
type execOutputTail struct {
	mw   *liveMaskWriter
	ring outputRing
}

// execOutputTails holds every kept tail by run id. The zero value is ready.
//
// An entry is pruned lazily, on the next open or read: TTL after its last
// output its buffer is released and it is marked expired, so a read can say
// so; after a second TTL the marker goes too. Memory is therefore bounded by
// the exec runs that printed within the last two TTLs.
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

// openExecOutput starts runID's tail and returns the writer dispatch hands the
// runner (runner.SandboxSpec.ExecOutput), or nil when this run keeps none.
func (s *Server) openExecOutput(run types.AgentRun, interactive bool) io.Writer {
	if s.cfg.ExecOutputTailOff || interactive || runIsUnrecordable(run) {
		return nil
	}
	runID := run.ID
	now := s.cfg.Now()
	e := &execOutputTail{ring: outputRing{max: s.cfg.RunOutputTailBytes, last: now, now: s.cfg.Now}}
	e.mw = &liveMaskWriter{reg: s.cfg.MaskRegistry, runID: runID, dst: &e.ring}
	t := &s.execOutputs
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now, s.cfg.ExecOutputTailTTL)
	if t.m == nil {
		t.m = map[uuid.UUID]*execOutputTail{}
	}
	t.m[runID] = e
	return e.mw
}

// readExecOutput returns at most limit bytes from the end of runID's tail.
// complete adds the bytes the masker is still holding back as a possible
// secret prefix, since no more output will complete them. kept is
// false when no tail is held; expired when one was and its TTL passed.
func (s *Server) readExecOutput(runID uuid.UUID, limit int, complete bool) (out []byte, truncated, kept, expired bool) {
	t := &s.execOutputs
	t.mu.Lock()
	t.pruneLocked(s.cfg.Now(), s.cfg.ExecOutputTailTTL)
	e := t.m[runID]
	t.mu.Unlock()
	if e == nil {
		return nil, false, false, false
	}
	e.mw.mu.Lock()
	defer e.mw.mu.Unlock()
	if e.ring.expired {
		return nil, false, true, true
	}
	out = append([]byte(nil), e.ring.buf...)
	if complete && len(e.mw.tail) > 0 {
		// The masker held these bytes back because a secret starts with them.
		// The run is over, so no more output completes one: a held-back run of
		// secretmask.MinLen bytes or more is a secret cut short and is hidden
		// (masking it against itself yields the placeholder); a shorter one is
		// ordinary output that shares a few leading bytes with a secret.
		out = append(out, secretmask.NewMasker([][]byte{e.mw.tail}).Mask(e.mw.tail)...)
	}
	truncated = e.ring.truncated
	if len(out) > limit {
		out, truncated = out[len(out)-limit:], true
	}
	return out, truncated, true, false
}

// runOutputResponse is GET /runs/{id}/output's body; client.RunOutput mirrors it.
type runOutputResponse struct {
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"` // output does not start at the run's first byte
	Complete  bool   `json:"complete"`  // the run has finished; bytes printed in its last moments can land a moment later
}

// handleRunOutput serves GET /api/v1/runs/{id}/output?tail=N — owner or admin,
// with GET /runs/{id}'s 404 for anyone else. No other answer here is a 404, so
// the status alone never reads as "you may not see this run".
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
	if run.Interactive {
		writeErrorReason(w, http.StatusConflict, reasonRunOutputInteractive,
			"an interactive run keeps no output here: its terminal is the recording's to keep")
		return
	}
	if s.cfg.ExecOutputTailOff {
		writeErrorReason(w, http.StatusConflict, reasonRunOutputOff,
			"this deployment keeps no run output (WARDYN_EXEC_OUTPUT_TAIL=off)")
		return
	}
	complete := run.State.IsTerminal()
	out, truncated, kept, expired := s.readExecOutput(id, limit, complete)
	switch {
	case expired:
		writeErrorReason(w, http.StatusGone, reasonRunOutputExpired,
			"this run's output has expired: it is kept for WARDYN_EXEC_OUTPUT_TAIL_TTL after the run's last output")
		return
	case !kept:
		writeErrorReason(w, http.StatusConflict, reasonRunOutputNotKept,
			"no output is kept for this run: only a non-interactive run started since wardynd last restarted keeps its output")
		return
	}
	writeJSON(w, http.StatusOK, runOutputResponse{Output: string(out), Truncated: truncated, Complete: complete})
}
