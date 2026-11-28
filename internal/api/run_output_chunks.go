// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The live exec tail in Postgres (ha-l2.4, migration 0129). A run's output tail lives in the
// ring of the replica that dispatched it (run_output.go). So that a read on any replica sees it,
// and so that what was printed survives that replica's crash, every byte the tail's masker
// forwards is also written to run_output_chunks:
//
//   - the bytes are the masked ones, taken from the writer's output after liveMaskWriter, so a
//     raw byte never reaches the table; the holdback the masker withholds as a possible secret
//     prefix is flushed into it on finalisation, by the same path that flushes it into the ring;
//   - the write is off the driver's path: tailSink queues the bytes and a goroutine writes them
//     in batches, so a slow database never blocks the runner's drain;
//   - the table keeps about one tail's worth per run (the store deletes the oldest chunks as it
//     appends), the final row's transaction deletes them, and a write honours the run's erasure
//     tombstone like every other run-output write;
//   - a runner run's live adoption replaces the chunks from its recovery tail (recoverChunks): it
//     re-reads the whole log into a fresh ring, so its first append clears the launcher's copy of
//     the same bytes. Every other recovery tail (run_output_recover.go) writes none.
//
// A read on another replica serves the chunks (readSharedOutput). When the replica that held the
// tail is gone and the substrate cannot be re-read, the run's row is written from the chunks,
// marked incomplete and a capture gap, since what the dead replica had not yet flushed is lost.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// runOutputChunkEvery is how often a tail's queued bytes are written.
	runOutputChunkEvery = 100 * time.Millisecond
	// runOutputChunkMax caps one chunk row.
	runOutputChunkMax = 32 << 10
	// runOutputChunkQueueMax caps what a tail queues while its writes fail: past it, or past the tail the
	// table keeps if that is larger, the oldest queued bytes are dropped (the ring still has them).
	runOutputChunkQueueMax = 256 << 10
	// runOutputChunkRetry is the wait after a failed write.
	runOutputChunkRetry = time.Second
	// runOutputChunkWriteTimeout bounds one write.
	runOutputChunkWriteTimeout = 10 * time.Second
)

// tailSink is the destination of a tail's masking writer: the ring, and the queue that mirrors it
// into run_output_chunks. Every write reaches it under the masking writer's mutex.
type tailSink struct {
	ring *outputRing
	q    *chunkQueue // nil when this tail keeps no shared chunks
}

func (t *tailSink) Write(p []byte) (int, error) {
	n, err := t.ring.Write(p)
	if t.q != nil {
		t.q.add(p)
	}
	return n, err
}

// chunkQueue batches a tail's masked bytes into run_output_chunks.
type chunkQueue struct {
	s    *Server
	run  uuid.UUID
	st   store.RunOutputChunkStore
	keep int

	wmu sync.Mutex // serialises writes, so chunks land in order
	// rs is set, before the queue's first write, when the queue is a live adoption's recovery tail: its appends
	// replace the run's chunks (recoverChunks). first is true until one of them writes; read and set under wmu.
	rs    store.RunOutputChunkReplacer
	first bool

	mu      sync.Mutex
	pend    []byte
	running bool
	stopped bool
}

func (q *chunkQueue) add(p []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return
	}
	q.pend = append(q.pend, p...)
	// Never below the tail the table keeps, or bytes inside it drop with nothing to say so.
	if over := len(q.pend) - max(runOutputChunkQueueMax, q.keep); over > 0 {
		q.pend = append(q.pend[:0], q.pend[over:]...)
	}
	if !q.running {
		q.running = true
		q.s.goBackground(q.loop)
	}
}

// take removes and returns what is queued.
func (q *chunkQueue) take() []byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	b := q.pend
	q.pend = nil
	return b
}

// putBack returns bytes a failed write could not store, ahead of anything queued since.
func (q *chunkQueue) putBack(b []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return
	}
	q.pend = append(b, q.pend...)
	// Never below the tail the table keeps, or bytes inside it drop with nothing to say so.
	if over := len(q.pend) - max(runOutputChunkQueueMax, q.keep); over > 0 {
		q.pend = append([]byte(nil), q.pend[over:]...)
	}
}

// stop ends the queue: nothing more is written, and what is queued is dropped. The ring has it.
func (q *chunkQueue) stop() {
	q.mu.Lock()
	clear(q.pend) // the fence calls this to erase what it holds, so zero it before dropping it
	q.stopped, q.pend = true, nil
	q.mu.Unlock()
}

func (q *chunkQueue) loop() {
	base := q.s.cfg.BaseCtx
	for {
		select {
		case <-base.Done():
			return
		case <-time.After(runOutputChunkEvery):
		}
		if err := q.flush(base); err != nil {
			select {
			case <-base.Done():
				return
			case <-time.After(runOutputChunkRetry):
			}
		}
		q.mu.Lock()
		if len(q.pend) == 0 || q.stopped {
			q.running = false
			q.mu.Unlock()
			return
		}
		q.mu.Unlock()
	}
}

// flush writes what is queued. An error leaves the bytes queued for the next try; an erasure
// tombstone fences the tail and ends the queue.
func (q *chunkQueue) flush(ctx context.Context) error {
	q.wmu.Lock()
	defer q.wmu.Unlock()
	b := q.take()
	for len(b) > 0 {
		n := min(len(b), runOutputChunkMax)
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runOutputChunkWriteTimeout)
		var wrote bool
		var err error
		if q.rs != nil {
			wrote, err = q.rs.AppendRecoveredRunOutputChunk(wctx, q.run, q.s.replicaName(), b[:n], q.keep, q.first)
			q.first = q.first && !wrote
		} else {
			wrote, err = q.st.AppendRunOutputChunk(wctx, q.run, q.s.replicaName(), b[:n], q.keep)
		}
		cancel()
		switch {
		case errors.Is(err, store.ErrRunOutputErased):
			q.stop()
			q.s.fenceRunOutput(q.run)
			return nil
		case err != nil:
			slog.Warn("wardynd: could not write a run's live output chunk; will retry",
				slog.String("run_id", q.run.String()), slog.Any("err", err))
			q.putBack(b)
			return err
		case !wrote:
			q.stop() // the final row exists, or a later adoption's recovery replaced this one: nothing more to keep
			return nil
		}
		b = b[n:]
	}
	return nil
}

// newTailSink is the destination for runID's tail ring: with a queue into run_output_chunks when
// the store keeps them and persistence is on.
func (s *Server) newTailSink(runID uuid.UUID, ring *outputRing) *tailSink {
	sink := &tailSink{ring: ring}
	if s.runOutputStore() == nil {
		return sink
	}
	if cs, ok := s.cfg.Store.(store.RunOutputChunkStore); ok {
		sink.q = &chunkQueue{s: s, run: runID, st: cs, keep: s.cfg.RunOutputTailBytes}
	}
	return sink
}

// dropChunks stops the tail mirroring itself into run_output_chunks.
func (e *execOutputTail) dropChunks() {
	e.mw.mu.Lock()
	defer e.mw.mu.Unlock()
	if e.sink != nil && e.sink.q != nil {
		e.sink.q.stop()
		e.sink.q = nil
	}
}

// recoverChunks makes a live adoption's recovery tail replace the run's chunks, when the store can
// (store.RunOutputChunkReplacer). Otherwise it stops the tail mirroring itself, as dropChunks does.
func (e *execOutputTail) recoverChunks() {
	e.mw.mu.Lock()
	defer e.mw.mu.Unlock()
	if e.sink == nil || e.sink.q == nil {
		return
	}
	if rs, ok := e.sink.q.st.(store.RunOutputChunkReplacer); ok {
		e.sink.q.rs, e.sink.q.first = rs, true // the queue is fresh: nothing has written yet
		return
	}
	e.sink.q.stop()
	e.sink.q = nil
}

// mirrored reports whether the tail's queue is a recovery's that wrote a chunk. The seal stopped the queue, so
// taking its write lock waits out an append in flight and none starts after it.
func (e *execOutputTail) mirrored() bool {
	e.mw.mu.Lock()
	var q *chunkQueue
	if e.sink != nil {
		q = e.sink.q
	}
	e.mw.mu.Unlock()
	if q == nil {
		return false
	}
	q.wmu.Lock()
	defer q.wmu.Unlock()
	return q.rs != nil && !q.first
}

// readSharedOutput answers a live read from the chunks another replica wrote, when this replica
// holds no tail for the run. true: the response is written. A run that ended longer ago than
// WARDYN_RUN_OUTPUT_RETENTION_DAYS on the database's clock (runOutputExpiredNow) is never answered
// from chunks, whatever a fault left of them: its output has expired, and the read's last answer says
// so. A clock that cannot be read answers a server error, so no chunk is served on a guess.
func (s *Server) readSharedOutput(w http.ResponseWriter, r *http.Request, run types.AgentRun, limit int) bool {
	cs, ok := s.cfg.Store.(store.RunOutputChunkStore)
	if !ok || s.runOutputStore() == nil {
		return false
	}
	if run.State.IsTerminal() {
		switch expired, err := s.runOutputExpiredNow(r.Context(), run); {
		case err != nil:
			writeServerError(w, r, "read the database's clock", err)
			return true
		case expired:
			return false
		}
	}
	id := run.ID
	ch, err := cs.ReadRunOutputChunks(r.Context(), id, limit)
	switch {
	case errors.Is(err, store.ErrRunOutputErased):
		s.fenceRunOutput(id)
		writeErrorReason(w, http.StatusNotFound, reasonRunOutputErased, "this run's output was erased")
		return true
	case err != nil:
		writeServerError(w, r, "read run output", err)
		return true
	case !ch.Found:
		return false
	case ch.Recovered && run.State.IsTerminal():
		return false // a recovery's chunks are never a terminal run's output: the pending row answers
	}
	s.writeRunOutput(w, r, id, runOutputResponse{
		Output: string(ch.Bytes), Truncated: ch.Truncated, Source: "stdout", MaskScope: s.liveMaskScope(false),
	})
	return true
}

// chunkSave is how saveRowFromChunks ended.
type chunkSave int

const (
	// chunksAbsent: the store answered that there are no chunks to save, that the run's row is
	// already final, or that the chunks are a recovery's this finaliser does not vouch for, so a
	// plain gap row loses nothing it may keep.
	chunksAbsent chunkSave = iota
	// chunksDone: the row was written from the chunks, or the output was erased and the tail fenced.
	chunksDone
	// chunksFailed: the save failed, so whether chunks exist is unknown. The pending row and any chunks
	// stay for the next try: a gap row now would delete bytes nothing else holds.
	chunksFailed
)

// saveRowFromChunks writes runID's row from the chunks a replica that is gone left, as an
// incomplete capture gap, and reports how that ended.
func (s *Server) saveRowFromChunks(ctx context.Context, runID uuid.UUID, reason string, own bool) chunkSave {
	cs, ok := s.cfg.Store.(store.RunOutputChunkStore)
	if !ok {
		return chunksAbsent
	}
	mine := "" // a recovery's chunks become a row only through its own covered seal
	if own {
		mine = s.replicaName() // this tail's covered seal vouches for its own chunks, never another recovery's
	}
	wrote, err := cs.SaveChunkedRunOutput(ctx, runID, mine, s.liveMaskScope(false), s.cfg.RunOutputTailBytes)
	switch {
	case errors.Is(err, store.ErrRunOutputErased):
		s.fenceRunOutput(runID)
		return chunksDone
	case err != nil:
		slog.WarnContext(ctx, "wardynd: could not write a run's row from its live chunks",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return chunksFailed
	case wrote:
		s.auditOutputFinalize(ctx, runID, map[string]any{"capture_gap": true, "incomplete": true, "reason": reason, "from_chunks": true})
		return chunksDone
	}
	return chunksAbsent
}
