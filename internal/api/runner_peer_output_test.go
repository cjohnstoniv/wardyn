// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// R23e (api): a yield while the tail's finaliser waits in awaitDrains on an open drain writes no row. The api
// answers remote.Options.OutputYielded with fenceRunOutput, then the remote sink finishes the drain with its yield
// error: the finaliser writes neither a final nor a gap row, the tail is gone and its chunk queue is stopped. A yield
// for a run with no tail frees nothing.
func TestPeerYieldDuringDrainWritesNoRow(t *testing.T) {
	f := newOutputFixture(t)
	w := f.open(t)
	writeExecOutput(t, w, "part\n")
	tail := f.srv.tailFor(f.run.ID)
	if tail == nil {
		t.Fatal("no tail for the run")
	}
	q := &chunkQueue{s: f.srv, run: f.run.ID}
	tail.mw.mu.Lock()
	tail.sink.q = q
	tail.mw.mu.Unlock()
	w.BeginDrain() // the runner's copy is still open
	done := make(chan struct{})
	go func() {
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond) // the finaliser waits on the drain (runOutputDrainWaitOverride is 2 s)

	f.srv.fenceRunOutput(f.run.ID) // OutputYielded
	w.EndDrain(errors.New("remote: the run's output went to the process that holds its watcher lease"))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the finaliser did not return after the yield")
	}
	if r, ok := f.mem.row(f.run.ID); ok && r.CapturedAt != nil {
		t.Fatalf("the yielded launcher wrote a row: %+v", r)
	}
	if n := f.mem.saveCount(f.run.ID); n != 0 {
		t.Fatalf("SaveFinalRunOutput ran %d times after the yield", n)
	}
	if f.srv.tailFor(f.run.ID) != nil {
		t.Fatal("the tail outlived the yield")
	}
	q.mu.Lock()
	stopped := q.stopped
	q.mu.Unlock()
	if !stopped {
		t.Fatal("the chunk queue was not stopped by the yield")
	}

	f.srv.fenceRunOutput(uuid.New()) // no tail: frees nothing, does not panic
}

// peerChunkStore is miscCovOutStore with store.RunOutputChunkReplacer on top. Each recovered append is recorded
// with its replica name; SaveChunkedRunOutput(…, mine, …) writes nothing, as the store's own transaction does, when
// one from a replica other than mine landed ("" excludes none), and ReadRunOutputChunks then serves the recovered
// bytes, Recovered set. It does not model a later adoption's refusal of an earlier one's append, which the store's
// PG test pins.
type peerChunkStore struct {
	*miscCovOutStore

	pmu      sync.Mutex
	recs     []miscCovAppend // each recovered append; replica is the name it was given
	firsts   []bool
	mines    []string // each SaveChunkedRunOutput call's mine
	recErr   error    // when set, SaveChunkedRunOutput fails with it
	gapFails int      // the next N SaveGapRunOutput calls fail
	gaps     int      // every SaveGapRunOutput call, failed ones included

	beforeSave func() // when set, runs as SaveChunkedRunOutput starts, before its check
}

var _ store.RunOutputChunkReplacer = (*peerChunkStore)(nil)

func (s *peerChunkStore) AppendRecoveredRunOutputChunk(_ context.Context, run uuid.UUID, replica string, b []byte, keep int, first bool) (bool, error) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.recs = append(s.recs, miscCovAppend{run, replica, bytes.Clone(b), keep})
	s.firsts = append(s.firsts, first)
	return true, nil
}

func (s *peerChunkStore) SaveChunkedRunOutput(ctx context.Context, run uuid.UUID, mine, scope string, limit int) (bool, error) {
	if s.beforeSave != nil {
		s.beforeSave()
	}
	s.pmu.Lock()
	s.mines = append(s.mines, mine)
	err := s.recErr
	other := slices.ContainsFunc(s.recs, func(a miscCovAppend) bool { return a.run == run && a.replica != mine })
	s.pmu.Unlock()
	if err != nil || other {
		return false, err
	}
	return s.miscCovOutStore.SaveChunkedRunOutput(ctx, run, mine, scope, limit)
}

func (s *peerChunkStore) ReadRunOutputChunks(ctx context.Context, run uuid.UUID, limit int) (store.RunOutputChunks, error) {
	ch, err := s.miscCovOutStore.ReadRunOutputChunks(ctx, run, limit)
	s.pmu.Lock()
	defer s.pmu.Unlock()
	for _, a := range s.recs {
		if a.run == run {
			if !ch.Recovered {
				ch.Bytes = nil
			}
			ch.Bytes, ch.Found, ch.Recovered = append(ch.Bytes, a.b...), true, true
		}
	}
	return ch, err
}

func (s *peerChunkStore) SaveGapRunOutput(ctx context.Context, id uuid.UUID) (bool, error) {
	s.pmu.Lock()
	s.gaps++
	fail := s.gapFails > 0
	if fail {
		s.gapFails--
	}
	s.pmu.Unlock()
	if fail {
		return false, errors.New("postgres is down")
	}
	return s.miscCovOutStore.SaveGapRunOutput(ctx, id)
}

func (s *peerChunkStore) recovered() int {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return len(s.recs)
}

func (s *peerChunkStore) mineCalls() []string {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return slices.Clone(s.mines)
}

func (s *peerChunkStore) gapCount() int {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return s.gaps
}

// chunkedSaves counts the SaveChunkedRunOutput calls that were not refused for a recovery's chunk.
func (s *peerChunkStore) chunkedSaves() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saveArgs)
}

// peerRecoverer is a runner whose RecoverOutput runs read, swappable between a test's phases.
type peerRecoverer struct {
	*outputRunner
	mu   sync.Mutex
	read func(ctx context.Context, w io.Writer) error
}

func (r *peerRecoverer) RecoverOutput(ctx context.Context, _ string, w io.Writer) error {
	r.mu.Lock()
	read := r.read
	r.mu.Unlock()
	return read(ctx, w)
}

func (r *peerRecoverer) set(read func(ctx context.Context, w io.Writer) error) {
	r.mu.Lock()
	r.read = read
	r.mu.Unlock()
}

// writeThenHold writes data, waits for release, and ends with end.
func writeThenHold(data string, release <-chan struct{}, end error) func(ctx context.Context, w io.Writer) error {
	return func(ctx context.Context, w io.Writer) error {
		if data != "" {
			_, _ = io.WriteString(w, data)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return end
	}
}

// peerOutputFixture is an output fixture for a runner run (ref runner:x) with a pending row, over peerChunkStore
// and a peerRecoverer. The launcher's chunks are there: SaveChunkedRunOutput writes.
func peerOutputFixture(t *testing.T) (*outputFixture, *peerChunkStore, *peerRecoverer) {
	t.Helper()
	var ps *peerChunkStore
	rr := &peerRecoverer{outputRunner: &outputRunner{fakeRunner: &fakeRunner{}}}
	f := newOutputFixture(t, func(c *Config) {
		ps = &peerChunkStore{miscCovOutStore: newMiscCovOutStore(c.Store.(*memRunOutputs))}
		c.Store, c.Runner = ps, rr
	})
	f.run.SandboxRef = "runner:x"
	f.st.mu.Lock()
	f.st.run.SandboxRef = "runner:x"
	f.st.mu.Unlock()
	f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: "stdout", ClaimedAt: time.Now().Add(-time.Hour)}
	ps.saveRes = true
	return f, ps, rr
}

// adopt is the live adoption of f's run on srv, through resumeRunOutput, with the recovery's read set to read.
// With appends, it waits for that many recovered appends to land.
func adopt(t *testing.T, f *outputFixture, srv *Server, ps *peerChunkStore, rr *peerRecoverer, read func(context.Context, io.Writer) error, appends int) *execOutputTail {
	t.Helper()
	rr.set(read)
	srv.resumeRunOutput(t.Context(), f.run.ID, "runner:x")
	e := srv.tailFor(f.run.ID)
	if e == nil {
		t.Fatal("the adoption opened no tail")
	}
	if appends > 0 {
		waitFor(t, "the recovery's append", func() bool { return ps.recovered() >= appends })
	}
	return e
}

// recoveryEnded waits until the tail has no open drain: the recovery's read returned and endRecovery ran.
func recoveryEnded(t *testing.T, e *execOutputTail) {
	t.Helper()
	waitFor(t, "the recovery's end", func() bool {
		e.dmu.Lock()
		defer e.dmu.Unlock()
		return e.drains == 0
	})
}

// loseCoverage puts a masking manifest store that cannot be read behind srv, so no run is covered.
func loseCoverage(t *testing.T, srv *Server) {
	t.Helper()
	pool, _ := miscCovClosedPool(t)
	srv.cfg.MaskManifests = maskmanifest.New(pool, nil, secretmask.NewRegistry())
}

// finalizeAudit is the one run.output.finalize row's reason and whether it has from_chunks.
func finalizeAudit(t *testing.T, f *outputFixture) (reason string, fromChunks bool) {
	t.Helper()
	rows := miscCovFinalizeData(t, f)
	if len(rows) != 1 {
		t.Fatalf("%d run.output.finalize rows, want one: %v", len(rows), rows)
	}
	r, _ := rows[0]["reason"].(string)
	return r, rows[0]["from_chunks"] == true
}

// R23g: a live adoption's own finaliser writes its chunks as the row only when the seal reports it covered and no
// other recovery's chunk is present.
func TestPeerAdoptUncoveredAtSealWritesPlainGap(t *testing.T) {
	type want struct {
		gaps, chunked int
		reason        string
		fromChunks    bool
	}
	cases := []struct {
		name     string
		data     string
		end      error
		lose     bool
		otherRec bool
		want     want
	}{
		{"coverage lost after an append", "part\n", nil, true, false, want{1, 0, "mask_uncovered", false}},
		{"coverage lost before any byte", "", nil, true, false, want{0, 1, "mask_uncovered", true}},
		{"runner offline, then coverage lost", "part\n", runner.ErrRunnerOffline, true, false, want{1, 0, "runner_offline", false}},
		{"runner offline, coverage kept", "part\n", runner.ErrRunnerOffline, false, false, want{0, 1, "runner_offline", true}},
		{"runner offline, another recovery's chunk", "part\n", runner.ErrRunnerOffline, false, true, want{1, 0, "runner_offline", false}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ps, rr := peerOutputFixture(t)
			release := make(chan struct{})
			appends := 0
			if tc.data != "" {
				appends = 1
			}
			e := adopt(t, f, f.srv, ps, rr, writeThenHold(tc.data, release, tc.end), appends)
			if tc.data == "" {
				time.Sleep(3 * runOutputChunkEvery) // no byte: no append can land
			}
			if tc.otherRec {
				_, _ = ps.AppendRecoveredRunOutputChunk(t.Context(), f.run.ID, "another-replica", []byte("x"), 1, true)
			}
			close(release)
			recoveryEnded(t, e)
			if tc.lose {
				loseCoverage(t, f.srv)
			}
			f.srv.FinishRunOutput(t.Context(), f.run.ID)

			if g, c := ps.gapCount(), ps.chunkedSaves(); g != tc.want.gaps || c != tc.want.chunked {
				t.Fatalf("case %d: SaveGapRunOutput %d, SaveChunkedRunOutput %d; want %d and %d", i+1, g, c, tc.want.gaps, tc.want.chunked)
			}
			if reason, fc := finalizeAudit(t, f); reason != tc.want.reason || fc != tc.want.fromChunks {
				t.Fatalf("case %d: audit reason %q from_chunks %v; want %q and %v", i+1, reason, fc, tc.want.reason, tc.want.fromChunks)
			}
			if i == 3 { // coverage kept: one read, with the tail's own replica name
				if m := ps.mineCalls(); len(m) != 1 || m[0] != f.srv.replicaName() {
					t.Fatalf("SaveChunkedRunOutput calls %q, want one with %q", m, f.srv.replicaName())
				}
			}
		})
	}
}

// R23h: a re-read at a run's end mirrors nothing, and its runner_offline end writes the row from the launcher's
// chunks.
func TestPeerRecoverAtRunEndMirrorsNoChunks(t *testing.T) {
	f, ps, rr := peerOutputFixture(t)
	held := make(chan struct{})
	rr.set(func(ctx context.Context, w io.Writer) error {
		_, _ = io.WriteString(w, "part\n")
		select {
		case <-time.After(5 * runOutputChunkEvery):
		case <-ctx.Done():
		}
		close(held)
		return runner.ErrRunnerOffline
	})
	f.srv.recoverRunOutput(t.Context(), ps, f.run, "no_tail")
	<-held
	if n, m := ps.recovered(), len(ps.appendLog()); n != 0 || m != 0 {
		t.Fatalf("the re-read appended %d recovered and %d plain chunks, want none", n, m)
	}
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	if c, g := ps.chunkedSaves(), ps.gapCount(); c != 1 || g != 0 {
		t.Fatalf("SaveChunkedRunOutput %d, SaveGapRunOutput %d; want the row from the launcher's chunks", c, g)
	}
	if reason, fc := finalizeAudit(t, f); reason != "runner_offline" || !fc {
		t.Fatalf("audit reason %q from_chunks %v, want runner_offline from the chunks", reason, fc)
	}
}

// uncoveredFailedGap is R23g's first case with its finaliser's gap write failing: the row stays pending, and the
// run is terminal.
func uncoveredFailedGap(t *testing.T) (*outputFixture, *peerChunkStore, *peerRecoverer) {
	t.Helper()
	f, ps, rr := peerOutputFixture(t)
	ps.gapFails = 1
	release := make(chan struct{})
	e := adopt(t, f, f.srv, ps, rr, writeThenHold("part\n", release, nil), 1)
	close(release)
	recoveryEnded(t, e)
	loseCoverage(t, f.srv)
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	if r, ok := f.mem.row(f.run.ID); !ok || r.CapturedAt != nil || ps.gapCount() != 1 {
		t.Fatalf("after the failed gap: row %+v (found %v), %d gap writes; want it pending after one", r, ok, ps.gapCount())
	}
	f.st.mu.Lock()
	f.st.state = types.RunFailed
	f.st.mu.Unlock()
	return f, ps, rr
}

// R23i: after its finaliser's gap write fails, a recovery's chunks never become the row, covered again or not.
func TestPeerUncoveredGapSkipsRecoveredChunks(t *testing.T) {
	t.Run("covered again, read ends sandbox_gone", func(t *testing.T) {
		f, ps, rr := uncoveredFailedGap(t)
		f.srv.cfg.MaskManifests = nil
		rr.set(func(context.Context, io.Writer) error { return runner.ErrSandboxGone })
		f.srv.resolveStalePending(t.Context(), ps, f.run.ID)
		if g, c := ps.gapCount(), ps.chunkedSaves(); g != 2 || c != 0 {
			t.Fatalf("SaveGapRunOutput %d, SaveChunkedRunOutput %d; want a second gap and no chunked row", g, c)
		}
		if reason, fc := finalizeAudit(t, f); reason != "sandbox_gone" || fc {
			t.Fatalf("audit reason %q from_chunks %v, want sandbox_gone and no from_chunks", reason, fc)
		}
	})
	t.Run("still uncovered", func(t *testing.T) {
		f, ps, _ := uncoveredFailedGap(t)
		f.srv.resolveStalePending(t.Context(), ps, f.run.ID)
		if g, c := ps.gapCount(), ps.chunkedSaves(); g != 2 || c != 0 {
			t.Fatalf("SaveGapRunOutput %d, SaveChunkedRunOutput %d; want a second gap and no chunked row", g, c)
		}
		if reason, fc := finalizeAudit(t, f); reason != "mask_uncovered" || fc {
			t.Fatalf("audit reason %q from_chunks %v, want mask_uncovered and no from_chunks", reason, fc)
		}
	})
	t.Run("the recovered-chunk read fails", func(t *testing.T) {
		f, ps, _ := uncoveredFailedGap(t)
		ps.pmu.Lock()
		ps.recErr = errors.New("postgres is down")
		ps.pmu.Unlock()
		f.srv.resolveStalePending(t.Context(), ps, f.run.ID)
		if g, c := ps.gapCount(), ps.chunkedSaves(); g != 1 || c != 0 {
			t.Fatalf("SaveGapRunOutput %d, SaveChunkedRunOutput %d; want neither written again", g, c)
		}
		if r, ok := f.mem.row(f.run.ID); !ok || r.CapturedAt != nil {
			t.Fatalf("row %+v (found %v), want it pending", r, ok)
		}
	})
	t.Run("the run's output was erased", func(t *testing.T) {
		f, ps, _ := uncoveredFailedGap(t)
		ps.pmu.Lock()
		ps.recErr = store.ErrRunOutputErased
		ps.pmu.Unlock()
		logs := miscCovCaptureLogs(t)
		f.srv.resolveStalePending(t.Context(), ps, f.run.ID)
		if g, c := ps.gapCount(), ps.chunkedSaves(); g != 1 || c != 0 {
			t.Fatalf("SaveGapRunOutput %d, SaveChunkedRunOutput %d; want no gap after the erasure", g, c)
		}
		if _, ok := logs.find("could not write a run's row from its live chunks"); ok {
			t.Fatal("an erased run's read was logged as a failure")
		}
		if f.srv.tailFor(f.run.ID) != nil {
			t.Fatal("the erased run kept a tail")
		}
	})
}

// An adoption whose first append lands while a finaliser writes a gone launcher's chunks as the row: the save
// refuses in its own check, so the finaliser writes a plain gap and no row from the recovery's chunks.
func TestPeerAdoptionDuringTheChunkSaveWritesPlainGap(t *testing.T) {
	f, ps, _ := peerOutputFixture(t)
	ps.beforeSave = func() {
		_, _ = ps.AppendRecoveredRunOutputChunk(t.Context(), f.run.ID, "adopting-replica", []byte("unsealed\n"), 1, true)
	}
	f.srv.writeGapRow(t.Context(), ps, f.run.ID, "replica_gone")
	if g, c := ps.gapCount(), ps.chunkedSaves(); g != 1 || c != 0 {
		t.Fatalf("SaveGapRunOutput %d, SaveChunkedRunOutput %d; want the plain gap and no row from the chunks", g, c)
	}
	if m := ps.mineCalls(); len(m) != 1 || m[0] != "" {
		t.Fatalf("SaveChunkedRunOutput calls %q, want one that vouches for no recovery", m)
	}
	if reason, fc := finalizeAudit(t, f); reason != "replica_gone" || fc {
		t.Fatalf("audit reason %q from_chunks %v, want replica_gone and no from_chunks", reason, fc)
	}
}

// R23j: the recovered chunks of a replica that took the run over and died serve another replica's live read only
// while the run runs, and never become the row.
func TestPeerDeadAdopterChunksNeverTheRow(t *testing.T) {
	f, ps, rr := peerOutputFixture(t)
	b := New(f.srv.cfg) // replica B: same store, no tail for the run
	adopt(t, f, f.srv, ps, rr, writeThenHold("recovered part\n", make(chan struct{}), nil), 1)
	// A stops without sealing: its tail stays in A, and B holds none.

	w := do(t, b, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/output", adminToken, "")
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("recovered part")) {
		t.Fatalf("B's live read of the running run: %d %s, want the recovered chunk", w.Code, w.Body)
	}
	f.st.mu.Lock()
	f.st.state = types.RunFailed
	f.st.mu.Unlock()
	w = do(t, b, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/output", adminToken, "")
	if w.Code != http.StatusConflict || !bytes.Contains(w.Body.Bytes(), []byte(reasonRunOutputNotKept)) || bytes.Contains(w.Body.Bytes(), []byte("recovered part")) {
		t.Fatalf("B's read of the terminal run: %d %s, want 409 %s and no chunk byte", w.Code, w.Body, reasonRunOutputNotKept)
	}

	run := f.run
	run.State = types.RunFailed
	rr.set(func(context.Context, io.Writer) error { return runner.ErrRunnerOffline })
	b.recoverRunOutput(t.Context(), ps, run, "stale_pending")
	b.finishRunOutput(t.Context(), run.ID)
	if c := ps.chunkedSaves(); c != 0 {
		t.Fatalf("SaveChunkedRunOutput ran %d times on a dead replica's recovered chunks", c)
	}
	for _, d := range miscCovFinalizeData(t, f) {
		if d["from_chunks"] == true {
			t.Fatalf("an audit row has from_chunks: %v", d)
		}
	}
	if ps.gapCount() != 1 {
		t.Fatalf("%d gap writes, want B's plain gap", ps.gapCount())
	}
}
