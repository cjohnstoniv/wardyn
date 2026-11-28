// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
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
