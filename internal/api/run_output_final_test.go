// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memRunOutputs is store.RunOutputStore over a map, in front of any other
// store: the contract's tombstone, pending/final and gap rules, with a switch to
// make the next final writes fail. The Postgres behaviour of the same rules is
// pinned in run_output_final_pg_test.go.
type memRunOutputs struct {
	store.Store
	mu       sync.Mutex
	rows     map[uuid.UUID]store.RunOutput
	erased   map[uuid.UUID]bool
	saves    map[uuid.UUID]int
	failSave int // the next N SaveFinalRunOutput calls fail
}

func newMemRunOutputs(inner store.Store) *memRunOutputs {
	return &memRunOutputs{Store: inner, rows: map[uuid.UUID]store.RunOutput{}, erased: map[uuid.UUID]bool{}, saves: map[uuid.UUID]int{}}
}

func (m *memRunOutputs) InsertPendingRunOutput(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.erased[id] {
		return store.ErrRunOutputErased
	}
	if _, ok := m.rows[id]; !ok {
		m.rows[id] = store.RunOutput{RunID: id, Source: "stdout", ClaimedAt: time.Now()}
	}
	return nil
}

func (m *memRunOutputs) RefreshRunOutputClaim(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.erased[id] {
		return store.ErrRunOutputErased
	}
	if r, ok := m.rows[id]; ok && r.CapturedAt == nil {
		r.ClaimedAt = time.Now()
		m.rows[id] = r
	}
	return nil
}

func (m *memRunOutputs) SaveFinalRunOutput(_ context.Context, o store.RunOutput) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.erased[o.RunID] {
		return store.ErrRunOutputErased
	}
	if m.failSave > 0 {
		m.failSave--
		return errors.New("postgres is down")
	}
	now := time.Now()
	o.CapturedAt, o.ClaimedAt = &now, now
	o.Output = append([]byte{}, o.Output...)
	m.rows[o.RunID] = o
	m.saves[o.RunID]++
	return nil
}

func (m *memRunOutputs) SaveGapRunOutput(_ context.Context, id uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.erased[id] {
		return false, store.ErrRunOutputErased
	}
	if r, ok := m.rows[id]; ok && r.CapturedAt != nil {
		return false, nil
	}
	now := time.Now()
	m.rows[id] = store.RunOutput{RunID: id, Source: "stdout", CaptureGap: true, CapturedAt: &now, ClaimedAt: now}
	return true, nil
}

func (m *memRunOutputs) MarkRunOutputIncomplete(_ context.Context, id uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.erased[id] {
		return false, store.ErrRunOutputErased
	}
	r, ok := m.rows[id]
	if !ok || r.CapturedAt == nil {
		return false, nil
	}
	r.Incomplete = true
	m.rows[id] = r
	return true, nil
}

func (m *memRunOutputs) GetRunOutput(_ context.Context, id uuid.UUID) (store.RunOutput, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.erased[id] {
		return store.RunOutput{}, false, store.ErrRunOutputErased
	}
	r, ok := m.rows[id]
	return r, ok, nil
}

func (m *memRunOutputs) EraseRunOutputs(_ context.Context, ids []uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		m.erased[id] = true
		delete(m.rows, id)
	}
	return nil
}

func (m *memRunOutputs) DeleteRunOutputsOlderThan(_ context.Context, age time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.rows {
		if r.CapturedAt != nil && time.Since(*r.CapturedAt) > age {
			delete(m.rows, id)
			n++
		}
	}
	return n, nil
}

func (m *memRunOutputs) ListStalePendingRunOutputs(ctx context.Context, age time.Duration, limit int) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []uuid.UUID
	for id, r := range m.rows {
		if r.CapturedAt != nil || time.Since(r.ClaimedAt) <= age || len(out) >= limit {
			continue
		}
		if run, err := m.Store.GetRun(ctx, id); err == nil && run.State.IsTerminal() {
			out = append(out, id)
		}
	}
	return out, nil
}

func (m *memRunOutputs) row(id uuid.UUID) (store.RunOutput, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	return r, ok
}

func (m *memRunOutputs) saveCount(id uuid.UUID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saves[id]
}

// outputRunner is a runner whose agent exits cleanly and which counts every
// call that would read the substrate, so a test can prove none was made.
type outputRunner struct {
	*fakeRunner
	mu    sync.Mutex
	reads int
}

func (r *outputRunner) Wait(context.Context, string) (int, error) { return 0, nil }
func (r *outputRunner) StopSandbox(context.Context, string) error { return nil }
func (r *outputRunner) KillSandbox(context.Context, string) error { return nil }
func (r *outputRunner) read()                                     { r.mu.Lock(); r.reads++; r.mu.Unlock() }
func (r *outputRunner) Status(context.Context, string) (runner.Status, error) {
	r.read()
	return runner.Status{}, errors.New("not expected")
}
func (r *outputRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	r.read()
	return runner.Status{}, errors.New("not expected")
}
func (r *outputRunner) ExecStream(context.Context, string, runner.ExecSpec) (*runner.ExecSession, error) {
	r.read()
	return nil, errors.New("not expected")
}
func (r *outputRunner) readCount() int { r.mu.Lock(); defer r.mu.Unlock(); return r.reads }

// outputFixture is one server whose store keeps run outputs in memory, a
// RUNNING non-interactive run seeded in it, and its recorders.
type outputFixture struct {
	srv   *Server
	mem   *memRunOutputs
	st    *dispatchTestStore
	audit *syncAudit
	rn    *outputRunner
	run   types.AgentRun
}

func newOutputFixture(t *testing.T, shape ...func(*Config)) *outputFixture {
	t.Helper()
	h := newHarness(t)
	run := newFinalizeRun()
	run.SandboxRef = "sbx-out"
	st := &dispatchTestStore{run: run, state: types.RunRunning}
	mem := newMemRunOutputs(st)
	audit := &syncAudit{}
	rn := &outputRunner{fakeRunner: &fakeRunner{}}
	baseCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := baseTestConfig(h, mem)
	cfg.Runner, cfg.Broker, cfg.Audit, cfg.BaseCtx = rn, &raceBroker{}, audit, baseCtx
	for _, f := range shape {
		f(&cfg)
	}
	srv := New(cfg)
	srv.runOutputRetryBaseOverride = time.Millisecond
	srv.runOutputDrainWaitOverride = 2 * time.Second
	return &outputFixture{srv: srv, mem: mem, st: st, audit: audit, rn: rn, run: run}
}

// open starts the run's tail and returns its writer as a driver holds it.
func (f *outputFixture) open(t *testing.T) *tailWriter {
	t.Helper()
	w, ok := f.srv.openExecOutput(f.run, false).(*tailWriter)
	if !ok {
		t.Fatal("openExecOutput returned no tail writer")
	}
	return w
}

func (f *outputFixture) get(t *testing.T) (int, runOutputResponse) {
	t.Helper()
	w := do(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/output", adminToken, "")
	var got runOutputResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %q: %v", w.Body, err)
		}
	}
	return w.Code, got
}

// finalRow waits for the run's final row.
func (f *outputFixture) finalRow(t *testing.T) store.RunOutput {
	t.Helper()
	var r store.RunOutput
	waitFor(t, "the final output row", func() bool {
		var ok bool
		r, ok = f.mem.row(f.run.ID)
		return ok && r.CapturedAt != nil
	})
	return r
}

// The row is masked before it is inserted, not at read: a registered secret
// printed whole, split across two writes, and held back as the final bytes is
// absent from the stored bytes themselves.
func TestFinishRunOutput_MasksBeforeInsert(t *testing.T) {
	reg := secretmask.NewRegistry()
	f := newOutputFixture(t, func(c *Config) { c.MaskRegistry = reg })
	reg.Add(f.run.ID, []byte("s3cr3t-token-value"))
	w := f.open(t)
	writeExecOutput(t, w, "whole s3cr3t-token-value here\n", "split s3cr3t-to", "ken-value done\n", "held s3cr3t-tok")
	f.srv.FinishRunOutput(t.Context(), f.run.ID)

	row := f.finalRow(t)
	stored := string(row.Output)
	if strings.Contains(stored, "s3cr3t-to") {
		t.Fatalf("the stored bytes hold the secret: %q", stored)
	}
	if want := "whole <secret-hidden> here\nsplit <secret-hidden> done\nheld <secret-hidden>"; stored != want {
		t.Fatalf("stored %q, want %q", stored, want)
	}
	if row.Incomplete || row.CaptureGap || row.Source != "stdout" {
		t.Errorf("a clean capture came out %+v", row)
	}
	if got := f.srv.tailFor(f.run.ID); got != nil {
		t.Error("the memory tail outlived its committed row")
	}
}

// A fake driver that writes after Wait returned but inside the barrier lands in
// the row, complete. Past the barrier the row is incomplete and the late bytes
// never reach it; a byte after the commit marks the committed row incomplete and
// audits it.
func TestFinishRunOutput_DrainBarrier(t *testing.T) {
	t.Run("writes inside the barrier land", func(t *testing.T) {
		f := newOutputFixture(t)
		w := f.open(t)
		writeExecOutput(t, w, "before exit\n")
		w.BeginDrain()
		go func() {
			time.Sleep(60 * time.Millisecond) // the process has exited; the copy has not reached EOF
			_, _ = w.Write([]byte("after exit\n"))
			w.EndDrain(nil)
		}()
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		row := f.finalRow(t)
		if string(row.Output) != "before exit\nafter exit\n" || row.Incomplete {
			t.Fatalf("row %q incomplete=%v, want both lines and a complete capture", row.Output, row.Incomplete)
		}
		if len(f.audit.eventsFor(f.run.ID, "run.output.finalize")) != 0 {
			t.Error("a clean capture wrote a run.output.finalize row")
		}
	})

	t.Run("past the barrier the row is incomplete and the late bytes never appear", func(t *testing.T) {
		f := newOutputFixture(t)
		f.srv.runOutputDrainWaitOverride = 30 * time.Millisecond
		w := f.open(t)
		writeExecOutput(t, w, "before exit\n")
		w.BeginDrain() // a copy that never ends in time
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		row := f.finalRow(t)
		if !row.Incomplete || string(row.Output) != "before exit\n" {
			t.Fatalf("row %q incomplete=%v, want the early bytes and incomplete", row.Output, row.Incomplete)
		}
		if n, err := w.Write([]byte("LATE\n")); err != nil || n != 5 {
			t.Fatalf("a write after the seal = %d, %v; the driver contract forbids failing it", n, err)
		}
		w.EndDrain(nil)
		waitFor(t, "the late write to be audited", func() bool {
			return len(f.audit.eventsFor(f.run.ID, "run.output.finalize")) >= 2
		})
		if r, _ := f.mem.row(f.run.ID); strings.Contains(string(r.Output), "LATE") {
			t.Fatalf("a byte after the seal reached the row: %q", r.Output)
		}
	})

	t.Run("a byte after the commit marks the row incomplete and is audited", func(t *testing.T) {
		f := newOutputFixture(t)
		w := f.open(t)
		writeExecOutput(t, w, "all of it\n")
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		if row := f.finalRow(t); row.Incomplete {
			t.Fatal("the row was incomplete before any late byte")
		}
		writeExecOutput(t, w, "one more\n", "and another\n")
		waitFor(t, "the row to turn incomplete", func() bool {
			r, _ := f.mem.row(f.run.ID)
			return r.Incomplete
		})
		evs := f.audit.eventsFor(f.run.ID, "run.output.finalize")
		if len(evs) != 1 || !strings.Contains(string(evs[0].Data), "late_write") {
			t.Fatalf("audit rows %v, want exactly one run.output.finalize naming the late write", evs)
		}
		if r, _ := f.mem.row(f.run.ID); string(r.Output) != "all of it\n" {
			t.Errorf("row %q, want the committed bytes untouched", r.Output)
		}
	})
}

// Every terminal path ends in exactly one final row per run, and finishing
// again, from any other path, writes nothing more.
func TestFinishRunOutput_EveryEntryPointYieldsOneRow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger func(t *testing.T, f *outputFixture)
	}{
		{"watcher win", func(t *testing.T, f *outputFixture) { f.srv.startCompletionWatcher(f.run.ID, "sbx-out", "exec-1") }},
		{"watcher CAS loss", func(t *testing.T, f *outputFixture) {
			f.st.mu.Lock()
			f.st.state = types.RunKilled
			f.st.mu.Unlock()
			f.srv.startCompletionWatcher(f.run.ID, "sbx-out", "exec-1")
		}},
		{"kill", func(t *testing.T, f *outputFixture) {
			w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/kill", adminToken, "")
			if w.Code != http.StatusAccepted {
				t.Fatalf("kill = %d %s", w.Code, w.Body)
			}
			f.srv.WaitBackground()
		}},
		{"idle stop", func(t *testing.T, f *outputFixture) { f.srv.FinishRunOutput(t.Context(), f.run.ID) }},
		{"lease end", func(t *testing.T, f *outputFixture) {
			f.srv.finalizeRunTail(t.Context(), f.run.ID, "sbx-out", "run.lease.ended", "success", map[string]any{})
		}},
		{"reconcile", func(t *testing.T, f *outputFixture) {
			f.srv.reconcileFinalize(t.Context(), f.run.ID, types.RunFailed, "sbx-out", "reconciled exit")
		}},
		{"dispatch failure", func(t *testing.T, f *outputFixture) {
			f.srv.failAndRevoke(t.Context(), f.run.ID, types.RunRunning, "the task could not start")
		}},
		{"probe reclaim", func(t *testing.T, f *outputFixture) { f.srv.reclaimProbeRun(t.Context(), f.run.ID) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOutputFixture(t)
			writeExecOutput(t, f.open(t), "line one\n", "line two\n")
			tc.trigger(t, f)
			row := f.finalRow(t)
			if string(row.Output) != "line one\nline two\n" || row.Incomplete || row.CaptureGap {
				t.Fatalf("row %+v, want both lines, clean", row)
			}
			f.srv.FinishRunOutput(t.Context(), f.run.ID)
			f.srv.finishRunOutput(t.Context(), f.run.ID)
			f.srv.WaitBackground()
			if n := f.mem.saveCount(f.run.ID); n != 1 {
				t.Fatalf("%d final writes, want exactly 1", n)
			}
		})
	}
}

// A kept run is not finished when its agent exits: the tail stays open, the
// revived agent's bytes join the ones from before, and ending the run writes one
// row holding both.
func TestFinishRunOutput_KeptRunRevivedThenEndedHasOneRow(t *testing.T) {
	f := newOutputFixture(t)
	w := f.open(t)
	w.BeginDrain()
	writeExecOutput(t, w, "before the lease ended\n")
	w.EndDrain(nil)
	f.st.mu.Lock()
	f.st.run.LostAt = &f.run.CreatedAt // kept: runIsKept
	f.st.mu.Unlock()
	f.srv.startCompletionWatcher(f.run.ID, "sbx-out", "exec-1")
	time.Sleep(100 * time.Millisecond) // the watcher's Wait returned; it must not have finalized
	if _, ok := f.mem.row(f.run.ID); !ok {
		t.Fatal("the pending row vanished")
	}
	if r, _ := f.mem.row(f.run.ID); r.CapturedAt != nil {
		t.Fatal("the watcher finished a kept run's output")
	}
	if f.srv.tailFor(f.run.ID) == nil {
		t.Fatal("the kept run's tail was released")
	}

	w.BeginDrain() // revived: the agent is started again on the same run id
	writeExecOutput(t, w, "after the revive\n")
	w.EndDrain(nil)
	f.st.mu.Lock()
	f.st.run.LostAt = nil
	f.st.mu.Unlock()
	f.srv.finalizeRunTail(t.Context(), f.run.ID, "sbx-out", "run.lease.ended", "success", map[string]any{})

	row := f.finalRow(t)
	if string(row.Output) != "before the lease ended\nafter the revive\n" || row.Incomplete {
		t.Fatalf("row %q incomplete=%v, want bytes from before and after the revive, complete", row.Output, row.Incomplete)
	}
	if n := f.mem.saveCount(f.run.ID); n != 1 {
		t.Fatalf("%d final writes, want 1", n)
	}
}

// A process that holds no tail for a run it finalises writes a capture gap with
// no bytes and reads nothing from the substrate.
func TestFinishRunOutput_NoTailWritesACaptureGap(t *testing.T) {
	f := newOutputFixture(t)
	f.srv.finalizeRunTail(t.Context(), f.run.ID, "sbx-out", "run.reconcile", "success", map[string]any{})
	row := f.finalRow(t)
	if !row.CaptureGap || len(row.Output) != 0 || row.CapturedAt == nil {
		t.Fatalf("row %+v, want a capture gap with no bytes", row)
	}
	if n := f.rn.readCount(); n != 0 {
		t.Fatalf("the finaliser made %d substrate reads, want none", n)
	}
	if len(f.audit.eventsFor(f.run.ID, "run.output.finalize")) != 1 {
		t.Error("a capture gap is not audited once")
	}
	code, got := f.get(t)
	if code != http.StatusOK || !got.CaptureGap || !got.Complete || got.Output != "" {
		t.Fatalf("read = %d %+v, want 200, a complete capture gap", code, got)
	}

	// A run that owes nothing gets no row.
	for name, mutate := range map[string]func(*types.AgentRun){
		"interactive":  func(r *types.AgentRun) { r.Interactive = true },
		"sign-in task": func(r *types.AgentRun) { r.Task = harnessLoginTask },
	} {
		g := newOutputFixture(t)
		mutate(&g.st.run)
		g.srv.FinishRunOutput(t.Context(), g.run.ID)
		if _, ok := g.mem.row(g.run.ID); ok {
			t.Errorf("%s run got an output row", name)
		}
	}
}

// A failing write is retried with backoff, bytes held in memory, until it lands;
// the failure is audited once.
func TestFinishRunOutput_RetriesAFailingWrite(t *testing.T) {
	f := newOutputFixture(t)
	writeExecOutput(t, f.open(t), "must not be lost\n")
	f.mem.mu.Lock()
	f.mem.failSave = 5 // more than the inline attempts, so the tracked loop finishes the job
	f.mem.mu.Unlock()
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	if r, _ := f.mem.row(f.run.ID); r.CapturedAt != nil {
		t.Fatal("the row committed while the store was failing")
	}
	if code, got := f.get(t); code != http.StatusOK || got.Output != "must not be lost\n" || !got.Complete {
		t.Fatalf("a read during the outage = %d %+v, want the bytes from memory, complete", code, got)
	}
	row := f.finalRow(t)
	if string(row.Output) != "must not be lost\n" || row.Incomplete {
		t.Fatalf("row %q incomplete=%v after the retries", row.Output, row.Incomplete)
	}
	evs := f.audit.eventsFor(f.run.ID, "run.output.finalize")
	if len(evs) != 1 || !strings.Contains(string(evs[0].Data), "persist_failing") {
		t.Fatalf("audit rows %v, want one run.output.finalize naming persist_failing", evs)
	}
	f.srv.WaitBackground()
}

// The settings matrix: master off collects nothing and refuses stored rows;
// persist off writes no rows and still serves stored ones; a row of a run that
// ended past retention reads 410 once it is gone.
func TestRunOutput_SettingsMatrix(t *testing.T) {
	t.Run("master off", func(t *testing.T) {
		f := newOutputFixture(t, func(c *Config) { c.ExecOutputTailOff = true })
		if f.srv.openExecOutput(f.run, false) != nil {
			t.Fatal("the master switch off still opened a tail")
		}
		if _, ok := f.mem.row(f.run.ID); ok {
			t.Fatal("the master switch off wrote a pending row")
		}
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		if _, ok := f.mem.row(f.run.ID); ok {
			t.Fatal("the master switch off wrote a final row")
		}
		now := time.Now()
		f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Output: []byte("old"), Source: "stdout", CapturedAt: &now}
		if code, _ := f.get(t); code != http.StatusConflict {
			t.Fatalf("a stored row with the master off = %d, want 409 %s", code, reasonRunOutputOff)
		}
	})

	t.Run("persist off", func(t *testing.T) {
		f := newOutputFixture(t, func(c *Config) { c.RunOutputPersistOff = true })
		writeExecOutput(t, f.open(t), "memory only\n")
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		if _, ok := f.mem.row(f.run.ID); ok {
			t.Fatal("persist off wrote a row")
		}
		if code, got := f.get(t); code != http.StatusOK || got.Output != "memory only\n" || !got.Complete {
			t.Fatalf("persist off read = %d %+v, want the memory tail, complete after the finish", code, got)
		}
		g := newOutputFixture(t, func(c *Config) { c.RunOutputPersistOff = true })
		now := time.Now()
		g.mem.rows[g.run.ID] = store.RunOutput{RunID: g.run.ID, Output: []byte("stored earlier"), Source: "stdout", CapturedAt: &now}
		if code, got := g.get(t); code != http.StatusOK || got.Output != "stored earlier" {
			t.Fatalf("persist off must still serve stored rows: %d %+v", code, got)
		}
	})

	t.Run("retention", func(t *testing.T) {
		f := newOutputFixture(t, func(c *Config) { c.RunOutputRetention = 30 * 24 * time.Hour })
		ended := time.Now().Add(-40 * 24 * time.Hour)
		f.st.mu.Lock()
		f.st.state, f.st.run.EndedAt = types.RunCompleted, &ended
		f.st.mu.Unlock()
		old := time.Now().Add(-35 * 24 * time.Hour)
		f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Output: []byte("stale"), Source: "stdout", CapturedAt: &old}
		if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.mem.row(f.run.ID); ok {
			t.Fatal("the sweep kept a row past the window")
		}
		w := do(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/output", adminToken, "")
		if w.Code != http.StatusGone || !strings.Contains(w.Body.String(), reasonRunOutputExpired) {
			t.Fatalf("a deleted row of an expired run = %d %s, want 410 %s", w.Code, w.Body, reasonRunOutputExpired)
		}
		if evs := f.audit.eventsFor(uuid.Nil, "run.output.retention.sweep"); len(evs) != 0 {
			t.Errorf("unexpected run-scoped rows %v", evs)
		}
	})
}

// The sweeper resolves a terminal run's stale pending row to a capture gap, and
// leaves one whose tail this process still holds.
func TestSweepRunOutputs_ResolvesStalePendingRows(t *testing.T) {
	f := newOutputFixture(t)
	f.st.mu.Lock()
	f.st.state = types.RunFailed
	f.st.mu.Unlock()
	f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: "stdout", ClaimedAt: time.Now().Add(-10 * time.Minute)}
	held := f.open(t) // this process still holds the tail: the finisher's
	_ = held
	f.mem.mu.Lock()
	f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: "stdout", ClaimedAt: time.Now().Add(-10 * time.Minute)}
	f.mem.mu.Unlock()
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.mem.row(f.run.ID); r.CapturedAt != nil {
		t.Fatal("the sweeper resolved a capture this process is still finishing")
	}
	f.srv.fenceRunOutput(f.run.ID) // the tail goes away (a restart)
	if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.mem.row(f.run.ID); !r.CaptureGap || r.CapturedAt == nil {
		t.Fatalf("row %+v, want the stale pending row resolved to a capture gap", r)
	}
}

// After an erasure, reads answer 404 run_output_erased, and a finaliser, a retry
// and a pending insert all fail to recreate the row. A second server holding the
// tail drops it, zeroed, at its next touch, asserted on the server struct.
func TestEraseRunOutputs(t *testing.T) {
	a := newOutputFixture(t)
	// B is a second server over the same store, holding a live tail for the run.
	cfgB := baseTestConfig(newHarness(t), a.mem)
	cfgB.Runner, cfgB.Audit = a.rn, &syncAudit{}
	b := New(cfgB)
	b.runOutputRetryBaseOverride = time.Millisecond
	wb, ok := b.openExecOutput(a.run, false).(*tailWriter)
	if !ok {
		t.Fatal("B opened no tail")
	}
	writeExecOutput(t, wb, "secret-ish output held on B\n")
	tail := b.tailFor(a.run.ID)
	tail.flushIn() // the write is in the ring, as a read would see it
	ring := tail.ring.buf
	if len(ring) == 0 {
		t.Fatal("B's ring is empty")
	}

	// A holds a blocked finaliser: the store fails, so its row sits in retry.
	writeExecOutput(t, a.open(t), "held on A\n")
	a.mem.mu.Lock()
	a.mem.failSave = 1 << 20
	a.mem.mu.Unlock()
	a.srv.FinishRunOutput(t.Context(), a.run.ID)

	if err := a.srv.EraseRunOutputs(t.Context(), []uuid.UUID{a.run.ID}); err != nil {
		t.Fatal(err)
	}
	if a.srv.tailFor(a.run.ID) != nil {
		t.Error("the erasing process kept its tail")
	}
	if w := do(t, a.srv, http.MethodGet, "/api/v1/runs/"+a.run.ID.String()+"/output", adminToken, ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), reasonRunOutputErased) {
		t.Fatalf("a read after the erasure = %d %s, want 404 %s", w.Code, w.Body, reasonRunOutputErased)
	}
	a.mem.mu.Lock()
	a.mem.failSave = 0 // the store recovers; the blocked finaliser's retry must still not recreate the row
	a.mem.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	a.srv.WaitBackground()
	if _, ok := a.mem.row(a.run.ID); ok {
		t.Fatal("a retry recreated the erased row")
	}
	if a.srv.openExecOutput(a.run, false) != nil {
		t.Error("a pending insert recreated a tail for an erased run")
	}

	// B still holds the tail until it touches the run; its first read drops and zeroes it.
	if b.tailFor(a.run.ID) == nil {
		t.Fatal("B lost its tail before touching the run")
	}
	if w := do(t, b, http.MethodGet, "/api/v1/runs/"+a.run.ID.String()+"/output", adminToken, ""); w.Code != http.StatusNotFound {
		t.Fatalf("B's read = %d, want 404", w.Code)
	}
	b.execOutputs.mu.Lock()
	_, held := b.execOutputs.m[a.run.ID]
	b.execOutputs.mu.Unlock()
	if held {
		t.Fatal("B's execOutputs still has the run after the erasure was seen")
	}
	if tail.ring.buf != nil || !bytes.Equal(ring, make([]byte, len(ring))) {
		t.Fatalf("B's ring buffer was not zeroed: %q", ring)
	}

	// And B's finaliser, had it run first, would have hit the tombstone too.
	wb2, _ := b.openExecOutput(a.run, false).(*tailWriter)
	if wb2 != nil {
		t.Fatal("B opened a tail for an erased run")
	}
	b.FinishRunOutput(t.Context(), a.run.ID)
	if _, ok := a.mem.row(a.run.ID); ok {
		t.Fatal("B's finaliser recreated the erased row")
	}
}

// A tail held by a server that has not yet seen an erasure is zeroed by its own
// finaliser's tombstone hit, not only by a read.
func TestEraseRunOutputs_FinaliserTouchFencesTheTail(t *testing.T) {
	f := newOutputFixture(t)
	tw := f.open(t)
	writeExecOutput(t, tw, "held\n")
	tail := f.srv.tailFor(f.run.ID)
	if err := f.mem.EraseRunOutputs(t.Context(), []uuid.UUID{f.run.ID}); err != nil { // another replica erased it
		t.Fatal(err)
	}
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	if f.srv.tailFor(f.run.ID) != nil || tail.ring.buf != nil {
		t.Fatal("the finaliser's tombstone hit left the tail in memory")
	}
	if _, ok := f.mem.row(f.run.ID); ok {
		t.Fatal("the finaliser wrote a row for an erased run")
	}
	if n, _ := tw.Write([]byte("late\n")); n != 5 {
		t.Fatal("a fenced writer must still accept writes")
	}
}
