// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type miscCovAppend struct {
	run     uuid.UUID
	replica string
	b       []byte
	keep    int
}

type miscCovSaveChunked struct {
	run   uuid.UUID
	scope string
	limit int
}

// miscCovOutStore is the in-memory run-output store with the live-chunk record on top, and a knob on
// every call a test needs to fail, count or hook. Calls are numbered from 1.
type miscCovOutStore struct {
	*memRunOutputs
	mu sync.Mutex

	// chunks
	appendFn func(call int, b []byte) (bool, error)
	appends  []miscCovAppend
	appended chan struct{}
	readRes  store.RunOutputChunks
	readErr  error
	reads    []int
	saveRes  bool
	saveErr  error
	saveArgs []miscCovSaveChunked

	// run outputs
	getRunErr      error
	gapErr         error
	deleteErr      error
	deleteN        int
	listErr        error
	listed         int
	saveFinalFn    func(call int) error
	saveFinalCalls int
	refreshFn      func(call int) error
	refreshCalls   int
	markErr        error
	eraseErr       error
	gapCalls       int
}

func newMiscCovOutStore(inner *memRunOutputs) *miscCovOutStore {
	return &miscCovOutStore{memRunOutputs: inner, appended: make(chan struct{}, 64)}
}

func (s *miscCovOutStore) AppendRunOutputChunk(_ context.Context, run uuid.UUID, replica string, b []byte, keep int) (bool, error) {
	s.mu.Lock()
	s.appends = append(s.appends, miscCovAppend{run, replica, bytes.Clone(b), keep})
	call := len(s.appends)
	fn := s.appendFn
	s.mu.Unlock()
	defer func() {
		select {
		case s.appended <- struct{}{}:
		default:
		}
	}()
	if fn != nil {
		return fn(call, b)
	}
	return true, nil
}

func (s *miscCovOutStore) appendLog() []miscCovAppend {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.appends)
}

func (s *miscCovOutStore) ReadRunOutputChunks(_ context.Context, _ uuid.UUID, limit int) (store.RunOutputChunks, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, limit)
	return s.readRes, s.readErr
}

func (s *miscCovOutStore) SaveChunkedRunOutput(_ context.Context, run uuid.UUID, scope string, limit int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveArgs = append(s.saveArgs, miscCovSaveChunked{run, scope, limit})
	return s.saveRes, s.saveErr
}

func (s *miscCovOutStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.getRunErr != nil {
		return types.AgentRun{}, s.getRunErr
	}
	return s.memRunOutputs.Store.GetRun(ctx, id)
}

func (s *miscCovOutStore) SaveGapRunOutput(ctx context.Context, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	s.gapCalls++
	err := s.gapErr
	s.mu.Unlock()
	if err != nil {
		return false, err
	}
	return s.memRunOutputs.SaveGapRunOutput(ctx, id)
}

func (s *miscCovOutStore) DeleteRunOutputsOlderThan(context.Context, time.Duration) (int, error) {
	return s.deleteN, s.deleteErr
}

func (s *miscCovOutStore) ListStalePendingRunOutputs(ctx context.Context, age time.Duration, limit int) ([]uuid.UUID, error) {
	s.mu.Lock()
	s.listed++
	err := s.listErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.memRunOutputs.ListStalePendingRunOutputs(ctx, age, limit)
}

func (s *miscCovOutStore) SaveFinalRunOutput(ctx context.Context, o store.RunOutput) error {
	s.mu.Lock()
	s.saveFinalCalls++
	call, fn := s.saveFinalCalls, s.saveFinalFn
	s.mu.Unlock()
	if fn != nil {
		if err := fn(call); err != nil {
			return err
		}
	}
	return s.memRunOutputs.SaveFinalRunOutput(ctx, o)
}

func (s *miscCovOutStore) RefreshRunOutputClaim(ctx context.Context, id uuid.UUID) error {
	s.mu.Lock()
	s.refreshCalls++
	call, fn := s.refreshCalls, s.refreshFn
	s.mu.Unlock()
	if fn != nil {
		if err := fn(call); err != nil {
			return err
		}
	}
	return s.memRunOutputs.RefreshRunOutputClaim(ctx, id)
}

func (s *miscCovOutStore) MarkRunOutputIncomplete(ctx context.Context, id uuid.UUID) (bool, error) {
	if s.markErr != nil {
		return false, s.markErr
	}
	return s.memRunOutputs.MarkRunOutputIncomplete(ctx, id)
}

func (s *miscCovOutStore) EraseRunOutputs(ctx context.Context, ids []uuid.UUID) error {
	if s.eraseErr != nil {
		return s.eraseErr
	}
	return s.memRunOutputs.EraseRunOutputs(ctx, ids)
}

// miscCovOutFixture is an output fixture whose store keeps live chunks and exposes the knobs above.
func miscCovOutFixture(t *testing.T, shape ...func(*Config)) (*outputFixture, *miscCovOutStore) {
	t.Helper()
	var cs *miscCovOutStore
	f := newOutputFixture(t, append([]func(*Config){func(c *Config) {
		cs = newMiscCovOutStore(c.Store.(*memRunOutputs))
		c.Store = cs
	}}, shape...)...)
	return f, cs
}

// miscCovQueue is the chunk queue of the fixture's open tail.
func miscCovQueue(t *testing.T, f *outputFixture) *chunkQueue {
	t.Helper()
	e := f.srv.tailFor(f.run.ID)
	if e == nil || e.sink == nil || e.sink.q == nil {
		t.Fatal("the run's tail has no chunk queue")
	}
	return e.sink.q
}

func miscCovPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func miscCovFinalizeData(t *testing.T, f *outputFixture) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.output.finalize") {
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// --- chunk queue -----------------------------------------------------------------------------

func TestMiscCovTailMirrorsIntoChunksOnlyWhenTheStoreKeepsThem(t *testing.T) {
	plain := newOutputFixture(t)
	plain.open(t)
	if q := plain.srv.tailFor(plain.run.ID).sink.q; q != nil {
		t.Error("a store with no chunk record gave the tail a chunk queue")
	}

	f, _ := miscCovOutFixture(t)
	f.open(t)
	q := miscCovQueue(t, f)
	if q.run != f.run.ID || q.keep != f.srv.cfg.RunOutputTailBytes {
		t.Errorf("queue is for run %s keeping %d, want %s keeping %d", q.run, q.keep, f.run.ID, f.srv.cfg.RunOutputTailBytes)
	}

	off, _ := miscCovOutFixture(t, func(c *Config) { c.RunOutputPersistOff = true })
	off.open(t)
	if q := off.srv.tailFor(off.run.ID).sink.q; q != nil {
		t.Error("persistence off still mirrored the tail into chunks")
	}
}

func TestMiscCovChunkFlushWritesInOrderInBoundedChunks(t *testing.T) {
	f, cs := miscCovOutFixture(t)
	f.open(t)
	q := miscCovQueue(t, f)
	data := miscCovPattern(2*runOutputChunkMax + 10)
	q.pend = bytes.Clone(data)

	if err := q.flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got := cs.appendLog()
	if len(got) != 3 || len(got[0].b) != runOutputChunkMax || len(got[1].b) != runOutputChunkMax || len(got[2].b) != 10 {
		t.Fatalf("chunk sizes = %v, want %d, %d and 10", func() []int {
			var n []int
			for _, a := range got {
				n = append(n, len(a.b))
			}
			return n
		}(), runOutputChunkMax, runOutputChunkMax)
	}
	var joined []byte
	for _, a := range got {
		joined = append(joined, a.b...)
		if a.run != f.run.ID || a.replica != f.srv.replicaName() || a.keep != f.srv.cfg.RunOutputTailBytes {
			t.Errorf("chunk written as %+v, want run %s from replica %q keeping %d", a, f.run.ID, f.srv.replicaName(), f.srv.cfg.RunOutputTailBytes)
		}
	}
	if !bytes.Equal(joined, data) {
		t.Error("the chunks do not add up to the queued bytes in order")
	}
	if len(q.take()) != 0 {
		t.Error("bytes are still queued after a complete flush")
	}
}

// A write that fails keeps the bytes it could not store, ahead of anything queued since, so the next
// flush writes them in order and nothing is lost or repeated.
func TestMiscCovChunkFlushKeepsWhatAFailedWriteCouldNotStore(t *testing.T) {
	f, cs := miscCovOutFixture(t)
	f.open(t)
	logs := miscCovCaptureLogs(t)
	q := miscCovQueue(t, f)
	boom := errors.New("postgres is down")
	cs.appendFn = func(call int, _ []byte) (bool, error) {
		if call == 2 {
			return false, boom
		}
		return true, nil
	}
	data := miscCovPattern(2*runOutputChunkMax + 10)
	q.pend = bytes.Clone(data)

	if err := q.flush(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("flush = %v, want the store's error", err)
	}
	if _, ok := logs.find("could not write a run's live output chunk"); !ok {
		t.Error("the failed write was not logged")
	}
	if !bytes.Equal(q.pend, data[runOutputChunkMax:]) {
		t.Fatalf("queued %d bytes after the failure, want the %d the store did not take", len(q.pend), len(data)-runOutputChunkMax)
	}

	cs.appendFn = nil
	if err := q.flush(t.Context()); err != nil {
		t.Fatalf("flush after recovery: %v", err)
	}
	var stored []byte
	for i, a := range cs.appendLog() {
		if i == 1 { // the failed attempt: nothing was stored
			continue
		}
		stored = append(stored, a.b...)
	}
	if !bytes.Equal(stored, data) {
		t.Error("what the store took, in order, is not the queued bytes exactly once")
	}
}

func TestMiscCovChunkFlushEndsTheQueueWhenTheStoreSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fn        func(int, []byte) (bool, error)
		wantFence bool
	}{
		{"an erasure tombstone fences the tail", func(int, []byte) (bool, error) { return false, store.ErrRunOutputErased }, true},
		{"a final row that exists ends the queue", func(int, []byte) (bool, error) { return false, nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cs := miscCovOutFixture(t)
			f.open(t)
			q := miscCovQueue(t, f)
			cs.appendFn = tc.fn
			q.pend = miscCovPattern(2 * runOutputChunkMax)

			if err := q.flush(t.Context()); err != nil {
				t.Fatalf("flush = %v, want nil", err)
			}
			if n := len(cs.appendLog()); n != 1 {
				t.Errorf("the store was written %d times, want 1: nothing is written after it says stop", n)
			}
			q.mu.Lock()
			stopped, pend := q.stopped, len(q.pend)
			q.mu.Unlock()
			if !stopped || pend != 0 {
				t.Errorf("stopped %v with %d bytes queued, want a stopped, empty queue", stopped, pend)
			}
			if fenced := f.srv.tailFor(f.run.ID) == nil; fenced != tc.wantFence {
				t.Errorf("tail dropped = %v, want %v", fenced, tc.wantFence)
			}
		})
	}
}

func TestMiscCovChunkQueueBoundsWhatItHolds(t *testing.T) {
	f, _ := miscCovOutFixture(t)
	q := &chunkQueue{s: f.srv, running: true} // running: no writer goroutine is started by add
	data := miscCovPattern(runOutputChunkQueueMax + 1000)

	q.add(data)
	if !bytes.Equal(q.pend, data[1000:]) {
		t.Fatalf("queued %d bytes, want the newest %d", len(q.pend), runOutputChunkQueueMax)
	}
	q.add([]byte("tail"))
	if len(q.pend) != runOutputChunkQueueMax || !bytes.HasSuffix(q.pend, []byte("tail")) {
		t.Errorf("after one more write the queue holds %d bytes ending %q, want the cap and the new bytes last", len(q.pend), q.pend[len(q.pend)-4:])
	}

	got := q.take()
	if len(got) != runOutputChunkQueueMax || len(q.pend) != 0 {
		t.Errorf("take returned %d bytes leaving %d, want everything and nothing", len(got), len(q.pend))
	}

	q.pend = []byte("new")
	q.putBack([]byte("old-"))
	if string(q.pend) != "old-new" {
		t.Errorf("after putBack the queue is %q, want the returned bytes ahead of the new", q.pend)
	}
	q.pend = bytes.Clone(data[:runOutputChunkQueueMax])
	q.putBack([]byte("0123456789"))
	if !bytes.Equal(q.pend, data[:runOutputChunkQueueMax]) {
		t.Errorf("an over-full putBack kept %d bytes, want the newest %d (the returned bytes trimmed from the front)", len(q.pend), runOutputChunkQueueMax)
	}

	q.stop()
	q.add([]byte("ignored"))
	q.putBack([]byte("ignored"))
	if len(q.pend) != 0 {
		t.Errorf("a stopped queue holds %d bytes, want none", len(q.pend))
	}
}

// Output written to a tail reaches the store as one chunk off the writer's path, tagged with the run,
// this replica and the tail size, and the queue's writer ends once nothing is left to write.
func TestMiscCovTailWritesReachTheChunkStore(t *testing.T) {
	f, cs := miscCovOutFixture(t)
	w := f.open(t)
	writeExecOutput(t, w, "hello")
	select {
	case <-cs.appended:
	case <-time.After(30 * time.Second):
		t.Fatal("the chunk was never written")
	}
	f.srv.WaitBackground()

	got := cs.appendLog()
	if len(got) != 1 || string(got[0].b) != "hello" || got[0].run != f.run.ID ||
		got[0].replica != f.srv.replicaName() || got[0].keep != f.srv.cfg.RunOutputTailBytes {
		t.Fatalf("chunks = %+v, want one chunk %q for run %s", got, "hello", f.run.ID)
	}
	q := miscCovQueue(t, f)
	q.mu.Lock()
	running := q.running
	q.mu.Unlock()
	if running {
		t.Error("the queue's writer is still marked running after it drained")
	}
}

// Bytes that arrive while a write is in flight are written next, in order, by the same writer.
func TestMiscCovChunkWriterGoesOnWhileBytesKeepArriving(t *testing.T) {
	f, cs := miscCovOutFixture(t)
	w := f.open(t)
	cs.appendFn = func(call int, _ []byte) (bool, error) {
		if call == 1 {
			_, _ = w.Write([]byte("more"))
		}
		return true, nil
	}
	writeExecOutput(t, w, "hello")
	// A write reaches the sink through the tail's input pipe; flushIn waits for that. "more" is written
	// from inside the first chunk write, so two chunk writes are the whole of what this test expects.
	f.srv.tailFor(f.run.ID).flushIn()
	for i := 0; i < 2; i++ {
		select {
		case <-cs.appended:
		case <-time.After(30 * time.Second):
			t.Fatalf("chunk write %d never happened", i+1)
		}
	}
	f.srv.WaitBackground()

	got := cs.appendLog()
	if len(got) != 2 || string(got[0].b) != "hello" || string(got[1].b) != "more" {
		t.Fatalf("chunks = %q, want hello then more, in order", func() []string {
			var s []string
			for _, a := range got {
				s = append(s, string(a.b))
			}
			return s
		}())
	}
}

// A daemon that is shutting down stops the writer: it writes nothing more, and a write that failed
// leaves its bytes queued rather than lost.
func TestMiscCovChunkWriterStopsWithTheDaemon(t *testing.T) {
	t.Run("before its first write", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f, cs := miscCovOutFixture(t, func(c *Config) { c.BaseCtx = ctx })
		cancel()
		writeExecOutput(t, f.open(t), "hello")
		f.srv.tailFor(f.run.ID).flushIn()
		f.srv.WaitBackground()
		if n := len(cs.appendLog()); n != 0 {
			t.Errorf("%d chunks written after shutdown, want none", n)
		}
	})
	t.Run("while waiting to retry a failed write", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		f, cs := miscCovOutFixture(t, func(c *Config) { c.BaseCtx = ctx })
		// The daemon stops while the first write is failing, so the writer is already past it when it
		// next looks at its context.
		cs.appendFn = func(call int, _ []byte) (bool, error) {
			if call == 1 {
				cancel()
			}
			return false, errors.New("postgres is down")
		}
		w := f.open(t)
		writeExecOutput(t, w, "hello")
		f.srv.tailFor(f.run.ID).flushIn()
		f.srv.WaitBackground()
		q := miscCovQueue(t, f)
		q.mu.Lock()
		pend := string(q.pend)
		q.mu.Unlock()
		if pend != "hello" || len(cs.appendLog()) != 1 {
			t.Errorf("queued %q after %d writes, want the failed bytes kept and no second try", pend, len(cs.appendLog()))
		}
	})
}

func TestMiscCovDropChunksStopsTheMirror(t *testing.T) {
	f, _ := miscCovOutFixture(t)
	f.open(t)
	e := f.srv.tailFor(f.run.ID)
	q := e.sink.q
	e.dropChunks()
	if e.sink.q != nil {
		t.Error("the tail still has a chunk queue after dropChunks")
	}
	q.add([]byte("late"))
	if len(q.pend) != 0 {
		t.Error("the dropped queue still takes bytes")
	}
	e.dropChunks() // a tail with no queue is a no-op

	plain := newOutputFixture(t)
	plain.open(t)
	plain.srv.tailFor(plain.run.ID).dropChunks()
}

// --- reading and saving from chunks --------------------------------------------------------------

func TestMiscCovReadSharedOutput(t *testing.T) {
	read := func(t *testing.T, srv *Server, id uuid.UUID, limit int) (*httptest.ResponseRecorder, bool) {
		t.Helper()
		w := httptest.NewRecorder()
		ok := srv.readSharedOutput(w, httptest.NewRequest(http.MethodGet, "/", nil), id, limit)
		return w, ok
	}

	t.Run("a store with no chunk record answers nothing", func(t *testing.T) {
		f := newOutputFixture(t)
		if w, ok := read(t, f.srv, f.run.ID, 10); ok || w.Body.Len() != 0 {
			t.Errorf("answered %v with %q", ok, w.Body)
		}
	})
	t.Run("persistence off answers nothing and reads nothing", func(t *testing.T) {
		f, cs := miscCovOutFixture(t, func(c *Config) { c.RunOutputPersistOff = true })
		if _, ok := read(t, f.srv, f.run.ID, 10); ok || len(cs.reads) != 0 {
			t.Errorf("answered %v after %d reads", ok, len(cs.reads))
		}
	})
	t.Run("an erased run is a 404 and its tail is fenced", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		f.open(t)
		cs.readErr = store.ErrRunOutputErased
		w, ok := read(t, f.srv, f.run.ID, 10)
		if !ok || w.Code != http.StatusNotFound || errorReason(w) != reasonRunOutputErased {
			t.Errorf("answered %v %d %s, want a 404 %s", ok, w.Code, w.Body, reasonRunOutputErased)
		}
		if f.srv.tailFor(f.run.ID) != nil {
			t.Error("the erased run's tail was not fenced")
		}
	})
	t.Run("a failed read is a 500", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.readErr = errors.New("read refused")
		if w, ok := read(t, f.srv, f.run.ID, 10); !ok || w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError {
			t.Errorf("answered %v %d %s, want a 500 %s", ok, w.Code, errorReason(w), reasonInternalError)
		}
	})
	t.Run("no chunks is for the caller to answer", func(t *testing.T) {
		f, _ := miscCovOutFixture(t)
		if w, ok := read(t, f.srv, f.run.ID, 10); ok || w.Body.Len() != 0 {
			t.Errorf("answered %v with %q", ok, w.Body)
		}
	})
	t.Run("chunks are served as the run's stdout", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.readRes = store.RunOutputChunks{Found: true, Bytes: []byte("masked output"), Truncated: true}
		w, ok := read(t, f.srv, f.run.ID, 4096)
		if !ok || w.Code != http.StatusOK {
			t.Fatalf("answered %v %d %s", ok, w.Code, w.Body)
		}
		var got runOutputResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Output != "masked output" || !got.Truncated || got.Source != "stdout" || got.Complete || got.MaskScope != "" {
			t.Errorf("response = %+v, want the chunk bytes, truncated, stdout, not complete", got)
		}
		if !slices.Equal(cs.reads, []int{4096}) {
			t.Errorf("read with limits %v, want [4096]", cs.reads)
		}
	})
}

func TestMiscCovSaveRowFromChunks(t *testing.T) {
	t.Run("a store with no chunk record writes nothing", func(t *testing.T) {
		f := newOutputFixture(t)
		if f.srv.saveRowFromChunks(t.Context(), f.run.ID, "no_tail") {
			t.Error("reported a row written")
		}
	})
	t.Run("a row from chunks is audited as an incomplete capture gap", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.saveRes = true
		if !f.srv.saveRowFromChunks(t.Context(), f.run.ID, "replica_gone") {
			t.Fatal("reported no row")
		}
		if len(cs.saveArgs) != 1 || cs.saveArgs[0].run != f.run.ID || cs.saveArgs[0].limit != f.srv.cfg.RunOutputTailBytes || cs.saveArgs[0].scope != "" {
			t.Errorf("store asked %+v, want this run, the tail size and no mask scope", cs.saveArgs)
		}
		got := miscCovFinalizeData(t, f)
		if len(got) != 1 || got[0]["from_chunks"] != true || got[0]["capture_gap"] != true || got[0]["incomplete"] != true || got[0]["reason"] != "replica_gone" {
			t.Errorf("audit = %v, want one row flagged from_chunks, capture_gap, incomplete with the reason", got)
		}
	})
	t.Run("no chunks to save is left to the plain gap row", func(t *testing.T) {
		f, _ := miscCovOutFixture(t)
		if f.srv.saveRowFromChunks(t.Context(), f.run.ID, "x") || len(miscCovFinalizeData(t, f)) != 0 {
			t.Error("reported or audited a row with nothing saved")
		}
	})
	t.Run("a failed save is logged and not reported", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.saveErr = errors.New("save refused")
		logs := miscCovCaptureLogs(t)
		if f.srv.saveRowFromChunks(t.Context(), f.run.ID, "x") {
			t.Error("reported a row written")
		}
		if _, ok := logs.find("could not write a run's row from its live chunks"); !ok {
			t.Error("the failure was not logged")
		}
	})
	t.Run("an erased run fences the tail and counts as handled", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		f.open(t)
		cs.saveErr = store.ErrRunOutputErased
		if !f.srv.saveRowFromChunks(t.Context(), f.run.ID, "x") {
			t.Error("an erased run was not treated as handled")
		}
		if f.srv.tailFor(f.run.ID) != nil || len(miscCovFinalizeData(t, f)) != 0 {
			t.Error("the tail was not fenced, or an erased run was audited as a gap")
		}
	})
}

func TestMiscCovWriteGapRow(t *testing.T) {
	t.Run("a row saved from chunks is the gap row", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.saveRes = true
		f.srv.writeGapRow(t.Context(), cs, f.run.ID, "no_tail")
		if cs.gapCalls != 0 {
			t.Error("a plain gap row was written over the row from chunks")
		}
	})
	t.Run("a plain gap row is audited with its reason", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		f.srv.writeGapRow(t.Context(), cs, f.run.ID, "no_tail")
		if r, ok := f.mem.row(f.run.ID); !ok || !r.CaptureGap {
			t.Errorf("row = %+v, want a capture gap", r)
		}
		if got := miscCovFinalizeData(t, f); len(got) != 1 || got[0]["capture_gap"] != true || got[0]["reason"] != "no_tail" {
			t.Errorf("audit = %v, want one gap row with the reason", got)
		}
	})
	t.Run("an erased run fences the tail and is not audited", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		f.open(t)
		cs.gapErr = store.ErrRunOutputErased
		f.srv.writeGapRow(t.Context(), cs, f.run.ID, "no_tail")
		if f.srv.tailFor(f.run.ID) != nil || len(miscCovFinalizeData(t, f)) != 0 {
			t.Error("the tail was not fenced, or an erased run was audited")
		}
	})
	t.Run("a failed write is logged and not audited", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.gapErr = errors.New("gap refused")
		logs := miscCovCaptureLogs(t)
		f.srv.writeGapRow(t.Context(), cs, f.run.ID, "no_tail")
		if _, ok := logs.find("could not write a run's capture-gap row"); !ok || len(miscCovFinalizeData(t, f)) != 0 {
			t.Error("the failure was not logged, or a gap that was never written was audited")
		}
	})
}

// --- finishing -------------------------------------------------------------------------------------

func TestMiscCovAwaitDrainsStopsWhenItsContextEnds(t *testing.T) {
	f := newOutputFixture(t)
	f.open(t)
	e := f.srv.tailFor(f.run.ID)
	e.beginDrain() // a copy that never ends
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if e.awaitDrains(ctx, time.Minute) {
		t.Error("awaitDrains reported clean with a copy still open and its context gone")
	}
}

func TestMiscCovLiveMaskScope(t *testing.T) {
	srv := newHarness(t).srv
	if got := srv.liveMaskScope(false); got != "" {
		t.Errorf("with no manifests the scope = %q, want none", got)
	}
	pool, _ := miscCovClosedPool(t)
	srv.cfg.MaskManifests = maskmanifest.New(pool, nil, secretmask.NewRegistry())
	if got := srv.liveMaskScope(false); got != maskScopeRun {
		t.Errorf("a covered writer's scope = %q, want %q", got, maskScopeRun)
	}
	if got := srv.liveMaskScope(true); got != maskScopeGlobalsOnly {
		t.Errorf("an uncovered writer's scope = %q, want %q", got, maskScopeGlobalsOnly)
	}
}

func TestMiscCovOutputRetryBaseFallsBackToTheConstant(t *testing.T) {
	srv := newHarness(t).srv
	if got := srv.outputRetryBase(); got != runOutputRetryBase {
		t.Errorf("retry base = %v, want %v", got, runOutputRetryBase)
	}
	srv.runOutputRetryBaseOverride = 7 * time.Millisecond
	if got := srv.outputRetryBase(); got != 7*time.Millisecond {
		t.Errorf("retry base with an override = %v", got)
	}
}

// A run whose writer cannot be proven covered by its masking manifest keeps what it can prove: the held
// bytes are dropped, the row says so, and its scope is globals only.
func TestMiscCovFinishWithAnUncoveredWriterDropsTheHeldBytes(t *testing.T) {
	reg := secretmask.NewRegistry()
	f, _ := miscCovOutFixture(t, func(c *Config) { c.MaskRegistry = reg })
	reg.Add(f.run.ID, []byte("s3cr3t-token-value"))
	w := f.open(t)
	writeExecOutput(t, w, "visible s3cr3t-tok")
	pool, _ := miscCovClosedPool(t)
	f.srv.cfg.MaskManifests = maskmanifest.New(pool, nil, reg) // its Covered read fails: not provable

	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	row := f.finalRow(t)
	if string(row.Output) != "visible " || row.MaskScope != maskScopeGlobalsOnly || !row.Incomplete {
		t.Fatalf("row output %q scope %q incomplete %v, want %q, %q, true", row.Output, row.MaskScope, row.Incomplete, "visible ", maskScopeGlobalsOnly)
	}
	got := miscCovFinalizeData(t, f)
	if len(got) != 1 || got[0]["dropped"] != true || got[0]["incomplete"] != true {
		t.Errorf("audit = %v, want one row flagged dropped and incomplete", got)
	}
}

// A recovery never persists a row masked by the globals alone: it writes a capture gap instead.
func TestMiscCovFinishOfARecoveredUncoveredTailWritesAGap(t *testing.T) {
	f, _ := miscCovOutFixture(t)
	f.open(t)
	e := f.srv.tailFor(f.run.ID)
	e.fmu.Lock()
	e.recovered = true
	e.fmu.Unlock()
	pool, _ := miscCovClosedPool(t)
	f.srv.cfg.MaskManifests = maskmanifest.New(pool, nil, secretmask.NewRegistry())

	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	row, ok := f.mem.row(f.run.ID)
	if !ok || !row.CaptureGap || len(row.Output) != 0 {
		t.Fatalf("row = %+v (found %v), want a capture gap with no bytes", row, ok)
	}
	if got := miscCovFinalizeData(t, f); len(got) != 1 || got[0]["reason"] != "mask_uncovered" || got[0]["capture_gap"] != true {
		t.Errorf("audit = %v, want one gap row with reason mask_uncovered", got)
	}
	if f.srv.tailFor(f.run.ID) != nil {
		t.Error("the memory tail outlived its gap row")
	}
}

// With persistence off there is no row to write; an incomplete capture is audited and nothing is stored.
func TestMiscCovFinishWithPersistenceOffAuditsAnIncompleteCapture(t *testing.T) {
	f, cs := miscCovOutFixture(t, func(c *Config) { c.RunOutputPersistOff = true })
	reg := secretmask.NewRegistry()
	f.srv.cfg.MaskRegistry = reg
	reg.Add(f.run.ID, []byte("s3cr3t-token-value"))
	writeExecOutput(t, f.open(t), "visible s3cr3t-tok")
	pool, _ := miscCovClosedPool(t)
	f.srv.cfg.MaskManifests = maskmanifest.New(pool, nil, reg)

	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	if _, ok := f.mem.row(f.run.ID); ok || cs.saveFinalCalls != 0 {
		t.Errorf("a row was written with persistence off (final saves %d)", cs.saveFinalCalls)
	}
	got := miscCovFinalizeData(t, f)
	if len(got) != 1 || got[0]["dropped"] != true || got[0]["incomplete"] != true {
		t.Errorf("audit = %v, want one incomplete row flagged dropped", got)
	}
}

func TestMiscCovFinishFromASecondCallerDoesNoneOfTheWork(t *testing.T) {
	f, cs := miscCovOutFixture(t)
	f.open(t)
	e := f.srv.tailFor(f.run.ID)
	e.fmu.Lock()
	e.started = true // another caller is finishing
	e.fmu.Unlock()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	f.srv.finishRunOutput(cancelled, f.run.ID)
	close(e.done)
	f.srv.finishRunOutput(t.Context(), f.run.ID)
	if cs.saveFinalCalls != 0 || cs.refreshCalls != 0 {
		t.Errorf("a second caller did the work: %d saves, %d claim refreshes", cs.saveFinalCalls, cs.refreshCalls)
	}
	if f.srv.tailFor(f.run.ID) == nil {
		t.Error("a second caller released the tail the first still owns")
	}
}

// An erasure that lands while the finisher is working fences the tail: no row is written.
func TestMiscCovFinishWritesNothingWhenTheOutputIsErasedMeanwhile(t *testing.T) {
	t.Run("at the claim refresh", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.refreshFn = func(int) error { return store.ErrRunOutputErased }
		writeExecOutput(t, f.open(t), "hello")
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		if cs.saveFinalCalls != 0 || f.srv.tailFor(f.run.ID) != nil {
			t.Errorf("final saves %d, tail kept %v; want none and a fenced tail", cs.saveFinalCalls, f.srv.tailFor(f.run.ID) != nil)
		}
	})
	t.Run("between the claim and the seal", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.refreshFn = func(int) error { f.srv.fenceRunOutput(f.run.ID); return nil }
		writeExecOutput(t, f.open(t), "hello")
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		if cs.saveFinalCalls != 0 {
			t.Errorf("%d final saves after the tail was fenced, want none", cs.saveFinalCalls)
		}
		if row, ok := f.mem.row(f.run.ID); ok && row.CapturedAt != nil {
			t.Errorf("a final row was written for an erased run: %+v", row)
		}
	})
	t.Run("at the final write", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.saveFinalFn = func(int) error { return store.ErrRunOutputErased }
		writeExecOutput(t, f.open(t), "hello")
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		if cs.saveFinalCalls != 1 || f.srv.tailFor(f.run.ID) != nil {
			t.Errorf("final saves %d, tail kept %v; want one attempt and a fenced tail", cs.saveFinalCalls, f.srv.tailFor(f.run.ID) != nil)
		}
		if len(miscCovFinalizeData(t, f)) != 0 {
			t.Error("an erased run was audited as a failed capture")
		}
	})
	t.Run("at the claim refresh after a failed write", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.saveFinalFn = func(int) error { return errors.New("postgres is down") }
		cs.refreshFn = func(call int) error {
			if call >= 2 { // the first is the terminal-time claim; the next follows the failed write
				return store.ErrRunOutputErased
			}
			return nil
		}
		writeExecOutput(t, f.open(t), "hello")
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		if cs.saveFinalCalls != 1 || f.srv.tailFor(f.run.ID) != nil {
			t.Errorf("final saves %d, tail kept %v; want one attempt and a fenced tail", cs.saveFinalCalls, f.srv.tailFor(f.run.ID) != nil)
		}
	})
}
