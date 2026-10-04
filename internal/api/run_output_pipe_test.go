// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tailOn is a run's tail on a server with no store, a tail big enough to keep a
// whole test stream, and reg as its masking registry.
func tailOn(t *testing.T, reg *secretmask.Registry) (*Server, uuid.UUID, *tailWriter) {
	t.Helper()
	srv := New(Config{MaskRegistry: reg, RunOutputTailBytes: 8 << 20, Audit: &recRecorder{}, AdminToken: adminToken})
	run := types.AgentRun{ID: uuid.New()}
	tw, _ := srv.openExecOutput(run, false).(*tailWriter)
	if tw == nil {
		t.Fatal("no tail for an exec run")
	}
	return srv, run.ID, tw
}

// finished is the tail as the finisher leaves it: drained, holdback flushed.
func finished(t *testing.T, srv *Server, id uuid.UUID) execOutputView {
	t.Helper()
	srv.FinishRunOutput(t.Context(), id)
	v, kept := srv.readExecOutput(id, 8<<20, true)
	if !kept || !v.complete {
		t.Fatalf("the finished tail: kept=%v complete=%v", kept, v.complete)
	}
	return v
}

// heldBackend is a readBackend whose reads wait for release.
type heldBackend struct {
	*readBackend
	release chan struct{}
}

func (h *heldBackend) Fresh(ctx context.Context, arrived time.Time) error {
	<-h.release
	return h.readBackend.Fresh(ctx, arrived)
}

// The Docker driver's copy is the agent's stdout drain: with one registry read
// per 50ms, an agent printing 5 MiB through a kernel pipe is held only by
// bandwidth, and the tail is the whole stream, masked, in order.
func TestRunOutputTail_AHeavyPrinterRunsAtBandwidthInOrder(t *testing.T) {
	reg, b := newReadBackend(50 * time.Millisecond)
	const value = "a-run-value-in-every-line"
	srv, id, tw := tailOn(t, reg)
	reg.AddLocal(id, []byte(value))

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	end := runner.BeginOutputDrain(tw)
	go func() {
		defer pr.Close()
		_, cerr := io.Copy(tw, pr)
		end(cerr)
	}()
	stream := heavyOutput(value)
	start := time.Now()
	for off := 0; off < len(stream); off += 4096 {
		if _, err := pw.Write(stream[off : off+4096]); err != nil {
			t.Fatal(err)
		}
	}
	_ = pw.Close()
	agent := time.Since(start)

	v := finished(t, srv, id)
	if !bytes.Equal(v.out, bytes.ReplaceAll(stream, []byte(value), []byte("<secret-hidden>"))) {
		t.Fatalf("the tail (%d bytes) is not the whole stream, masked, in order", len(v.out))
	}
	// One read per 32 KiB copy was 160 reads, 8s.
	if agent > 3*time.Second {
		t.Errorf("the agent took %v to print 5 MiB", agent)
	}
	if n := b.readCount(); n > 80 {
		t.Errorf("%d registry reads for 160 copies, want batches", n)
	}
}

// EndDrain returns at once, while the copy's bytes are still waiting for the
// masker, and the drain ends only once they are in the tail: the finisher never
// seals ahead of them.
func TestRunOutputTail_EndDrainNeverBlocksAndEndsAfterItsBytes(t *testing.T) {
	reg, rb := newReadBackend(0)
	held := &heldBackend{readBackend: rb, release: make(chan struct{})}
	reg.SetBackend(held)
	srv, id, tw := tailOn(t, reg)

	tw.BeginDrain()
	writeExecOutput(t, tw, "masked once the read answers\n")
	returned := make(chan struct{})
	go func() {
		tw.EndDrain(nil)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("EndDrain blocked on the masker")
	}
	if tw.t.awaitDrains(t.Context(), 100*time.Millisecond) {
		t.Fatal("the drain ended before its bytes were in the tail")
	}
	close(held.release)
	if !tw.t.awaitDrains(t.Context(), 5*time.Second) {
		t.Fatal("the drain did not end once the masker had its bytes")
	}
	if v := finished(t, srv, id); string(v.out) != "masked once the read answers\n" {
		t.Fatalf("the tail = %q", v.out)
	}
}

// A value committed on another replica before the bytes arrived is masked in
// the tail: a batch's read began after every byte in it arrived.
func TestRunOutputTail_AValueCommittedElsewhereBeforeTheBytesIsMasked(t *testing.T) {
	reg, b := newReadBackend(20 * time.Millisecond)
	srv, id, tw := tailOn(t, reg)
	const value = "committed-on-another-replica-88"
	b.stage(id, value)
	if strings.Contains(string(reg.Masker(id).Mask([]byte(value))), "<secret-hidden>") {
		t.Fatal("this process already holds the value: the test would prove nothing")
	}
	for range 20 {
		writeExecOutput(t, tw, "printed: "+value+"\n")
	}
	if v := finished(t, srv, id); bytes.Contains(v.out, []byte(value)) || !bytes.Contains(v.out, []byte("<secret-hidden>")) {
		t.Fatalf("a value committed before its bytes arrived was not masked: %q", v.out)
	}
}

// A secret split across two batches is held back and masked whole, and a value
// cut short at the end is hidden when the finisher releases the holdback.
func TestRunOutputTail_SplitAndHeldBackValuesAreMasked(t *testing.T) {
	reg, _ := newReadBackend(0)
	srv, id, tw := tailOn(t, reg)
	const value = "split-across-two-batches-value"
	reg.AddLocal(id, []byte(value))
	half := len(value) / 2
	writeExecOutput(t, tw, "split "+value[:half])
	tw.t.flushIn() // the first batch ends mid-value
	writeExecOutput(t, tw, value[half:]+" ok\nheld "+value[:12])
	if v := finished(t, srv, id); string(v.out) != "split <secret-hidden> ok\nheld <secret-hidden>" {
		t.Fatalf("the tail = %q", v.out)
	}
}

// With the registry unreadable a batch is replaced by the placeholder, and the
// capture says it dropped bytes and was not masked against the whole corpus.
func TestRunOutputTail_AnUnreadableRegistryFailsTheBatchClosed(t *testing.T) {
	reg, b := newReadBackend(0)
	srv, id, tw := tailOn(t, reg)
	writeExecOutput(t, tw, "healthy\n")
	tw.t.flushIn()
	b.mu.Lock()
	b.down = true
	b.mu.Unlock()
	for range 10 {
		writeExecOutput(t, tw, "output the server cannot vouch for\n")
	}
	v := finished(t, srv, id)
	if got := string(v.out); strings.Contains(got, "vouch") || !strings.HasPrefix(got, "healthy\n<secret-hidden>") {
		t.Fatalf("with the registry unreadable the tail = %q", got)
	}
	e := tw.t
	e.mw.mu.Lock()
	defer e.mw.mu.Unlock()
	if !e.mw.capture.dropped || !e.mw.capture.uncovered {
		t.Errorf("the capture did not record the replaced batch: %+v", e.mw.capture)
	}
}
