// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A batch run's output tail under heavy output, against the Postgres masking
// registry. Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when
// unset.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// heavyOutput is 5 MiB of 4 KiB lines, each numbered and carrying value.
func heavyOutput(value string) []byte {
	var b bytes.Buffer
	for i := range 1280 {
		line := fmt.Sprintf("line %06d %s ", i, value)
		b.WriteString(line)
		b.Write(bytes.Repeat([]byte("x"), 4096-len(line)-1))
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// wantTail is the last n bytes of stream with value masked: what a tail of n
// bytes holds once the whole stream went through it.
func wantTail(stream []byte, value string, n int) []byte {
	masked := bytes.ReplaceAll(stream, []byte(value), []byte("<secret-hidden>"))
	return masked[len(masked)-n:]
}

// The Docker exec driver drains the agent's terminal with io.Copy into the run's
// tail and nothing else reads it, so the tail's Write is the agent's stdout: an
// agent printing 5 MiB through a kernel pipe finishes at bandwidth, not at one
// masking-registry read (at most one per 50ms per replica) per 32 KiB copy.
func TestRunOutputPG_AnAgentPrintingHeavilyIsNotHeldToTheReadRate(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	const value = "dispatch-time-value-6"
	a.dispatch(t, run, value)
	w := b.srv.openExecOutput(run, false)
	if w == nil {
		t.Fatal("no tail for an exec run")
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	end := runner.BeginOutputDrain(w)
	go func() {
		defer pr.Close()
		_, cerr := io.Copy(w, pr)
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
	t.Logf("the agent printed %d bytes in %v: %.1f MB/s", len(stream), agent, float64(len(stream))/agent.Seconds()/1e6)
	// The old per-write read held this near 0.7 MB/s (about 8s).
	if agent > 3*time.Second {
		t.Errorf("the agent took %v to print 5 MiB: its stdout is held to the registry read rate", agent)
	}

	b.srv.FinishRunOutput(t.Context(), run.ID)
	raw, final := l.storedOutput(run.ID)
	if !final || !bytes.Equal(raw, wantTail(stream, value, len(raw))) || len(raw) != b.srv.cfg.RunOutputTailBytes {
		t.Fatalf("the persisted row (%d bytes, final=%v) is not the stream's masked tail", len(raw), final)
	}
	if n := l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND incomplete`, run.ID); n != 0 {
		t.Error("a capture that drained in full was marked incomplete")
	}
}

// The Kubernetes driver and an exec-less Docker agent follow a log the runtime
// keeps, so the agent is not held, but the copy is: an agent that printed 5 MiB
// and exited is captured in full only if the copy catches up inside the
// finisher's drain wait.
func TestRunOutputPG_AHeavyLogIsCapturedCompleteAtExit(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	const value = "dispatch-time-value-7"
	a.dispatch(t, run, value)
	w := b.srv.openExecOutput(run, false)
	if w == nil {
		t.Fatal("no tail for an exec run")
	}
	stream := heavyOutput(value)
	end := runner.BeginOutputDrain(w)
	start := time.Now()
	go func() {
		// A log stream arrives in reads, not as one buffer handed over whole.
		_, cerr := io.Copy(w, struct{ io.Reader }{bytes.NewReader(stream)})
		end(cerr)
	}()

	b.srv.FinishRunOutput(t.Context(), run.ID) // the agent has exited
	t.Logf("captured and persisted %d bytes in %v", len(stream), time.Since(start))
	raw, final := l.storedOutput(run.ID)
	if n := l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND incomplete`, run.ID); n != 0 {
		t.Fatal("the capture of a 5 MiB log was marked incomplete: the copy did not drain inside the finisher's wait")
	}
	if !final || !bytes.Equal(raw, wantTail(stream, value, len(raw))) {
		t.Fatalf("the persisted row (%d bytes, final=%v) is not the stream's masked tail", len(raw), final)
	}
}
