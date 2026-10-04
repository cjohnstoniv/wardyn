// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// readBackend is a shared corpus whose every Fresh is a read that begins when it
// is called and takes gap, as the Postgres registry's coalesced read does. A
// value staged with stage is committed "on another replica": this process's
// cache gets it only from a read that began after the commit.
type readBackend struct {
	reg *secretmask.Registry
	gap time.Duration

	mu     sync.Mutex
	reads  int
	down   bool
	staged []readBackendValue
}

type readBackendValue struct {
	run   uuid.UUID
	value []byte
	at    time.Time
}

func (b *readBackend) stage(run uuid.UUID, value string) {
	b.mu.Lock()
	b.staged = append(b.staged, readBackendValue{run, []byte(value), time.Now()})
	b.mu.Unlock()
}

func (b *readBackend) readCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

func (b *readBackend) Fresh(_ context.Context, _ time.Time) error {
	start := time.Now()
	time.Sleep(b.gap)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	if b.down {
		return errors.New("postgres is unreachable")
	}
	left := b.staged[:0]
	for _, v := range b.staged {
		if v.at.After(start) {
			left = append(left, v)
			continue
		}
		b.reg.AddLocal(v.run, v.value)
	}
	b.staged = left
	return nil
}

func (b *readBackend) PutRun(uuid.UUID, []byte) error { return nil }
func (b *readBackend) PutGlobal(string, string, []secretmask.GlobalPut, bool, time.Time) error {
	return nil
}
func (b *readBackend) EvictGlobal(string, string, time.Time) error          { return nil }
func (b *readBackend) SweepGlobals(context.Context, time.Time) (int, error) { return 0, nil }
func (b *readBackend) PersistedRuns(context.Context) ([]uuid.UUID, error)   { return nil, nil }
func (b *readBackend) PurgeRuns(context.Context, []uuid.UUID) error         { return nil }
func (b *readBackend) EraseOwner(context.Context, string) (int, error)      { return 0, nil }

func newReadBackend(gap time.Duration) (*secretmask.Registry, *readBackend) {
	reg := secretmask.NewRegistry()
	b := &readBackend{reg: reg, gap: gap}
	reg.SetBackend(b)
	return reg, b
}

// endPipe flushes the pipe and then the masker's holdback, as finishRecording does.
func endPipe(p *maskPipe) {
	p.flush()
	p.mw.mu.Lock()
	p.mw.flushLocked()
	p.mw.mu.Unlock()
}

// The pump never waits on a registry read per chunk: chunks that arrive while a
// read runs are masked together after the next one, in order, so 300 chunks
// cost a handful of reads rather than 300.
func TestMaskPipe_BatchesChunksInOrderAcrossSlowReads(t *testing.T) {
	const gap = 50 * time.Millisecond
	reg, b := newReadBackend(gap)
	run := uuid.New()
	const secret = "a-registered-run-secret-0001"
	reg.AddLocal(run, []byte(secret))
	dst := &lockedBuffer{}
	p := newMaskPipe(&liveMaskWriter{reg: reg, runID: run, dst: dst})

	const chunks = 300
	var want strings.Builder
	start := time.Now()
	for i := range chunks {
		line := fmt.Sprintf("line %04d %s\n", i, strings.Repeat("y", 1000))
		if i%50 == 7 {
			line = fmt.Sprintf("line %04d token=%s\n", i, secret)
		}
		_, _ = p.Write([]byte(line))
		want.WriteString(strings.ReplaceAll(line, secret, "<secret-hidden>"))
	}
	queued := time.Since(start)
	endPipe(p)

	if got := dst.String(); got != want.String() {
		t.Fatalf("the masked stream differs from the input, masked, in order (got %d bytes, want %d)", len(got), want.Len())
	}
	// The old writer waited one read per chunk: chunks*gap = 15s.
	if queued > chunks*gap/10 {
		t.Errorf("queueing %d chunks took %v; the producer waited on the masker's reads", chunks, queued)
	}
	if n := b.readCount(); n > chunks/10 {
		t.Errorf("%d registry reads for %d chunks, want batches", n, chunks)
	}
}

// A value committed on another replica before the bytes arrived is masked: the
// batch's read began after every byte in it arrived.
func TestMaskPipe_AValueCommittedElsewhereBeforeTheBytesIsMasked(t *testing.T) {
	reg, b := newReadBackend(20 * time.Millisecond)
	run := uuid.New()
	dst := &lockedBuffer{}
	p := newMaskPipe(&liveMaskWriter{reg: reg, runID: run, dst: dst})

	// A batch in flight, so the value's chunk joins a later one.
	_, _ = p.Write([]byte("before the commit\n"))
	const secret = "committed-on-another-replica-77"
	b.stage(run, secret)
	if strings.Contains(string(reg.Masker(run).Mask([]byte(secret))), "<secret-hidden>") {
		t.Fatal("this process already holds the value: the test would prove nothing")
	}
	for range 20 {
		_, _ = p.Write([]byte("printed: " + secret + "\n"))
	}
	endPipe(p)
	if got := dst.String(); strings.Contains(got, secret) || !strings.Contains(got, "<secret-hidden>") {
		t.Fatalf("a value committed before its bytes arrived was not masked: %q", got)
	}
}

// A secret split across two batches is held back at the end of the first and
// masked whole in the second.
func TestMaskPipe_ASecretSplitAcrossABatchBoundaryIsMasked(t *testing.T) {
	reg, _ := newReadBackend(0)
	run := uuid.New()
	const secret = "split-across-two-batches-secret"
	reg.AddLocal(run, []byte(secret))
	dst := &lockedBuffer{}
	p := newMaskPipe(&liveMaskWriter{reg: reg, runID: run, dst: dst})

	half := len(secret) / 2
	_, _ = p.Write([]byte("prefix " + secret[:half]))
	p.flush() // the first batch ends mid-secret
	if got := dst.String(); got != "prefix " {
		t.Fatalf("after the first batch the writer forwarded %q, want only the bytes before the held-back prefix", got)
	}
	_, _ = p.Write([]byte(secret[half:] + " suffix"))
	endPipe(p)
	if got := dst.String(); got != "prefix <secret-hidden> suffix" {
		t.Fatalf("a secret split across two batches = %q", got)
	}
}

// With the registry unreadable the batch is replaced by the placeholder: no byte
// the server cannot vouch for is forwarded.
func TestMaskPipe_AnUnreadableRegistryFailsTheBatchClosed(t *testing.T) {
	reg, b := newReadBackend(0)
	run := uuid.New()
	dst := &lockedBuffer{}
	mw := &liveMaskWriter{reg: reg, runID: run, dst: dst}
	p := newMaskPipe(mw)
	_, _ = p.Write([]byte("healthy output\n"))
	p.flush()

	b.mu.Lock()
	b.down = true
	b.mu.Unlock()
	for range 10 {
		_, _ = p.Write([]byte("output the server cannot vouch for\n"))
	}
	endPipe(p)
	got := dst.String()
	if strings.Contains(got, "vouch") || !strings.HasPrefix(got, "healthy output\n") || !strings.HasSuffix(got, "<secret-hidden>") {
		t.Fatalf("with the registry unreadable the recording got %q", got)
	}
	mw.mu.Lock()
	defer mw.mu.Unlock()
	if !mw.capture.dropped || !mw.capture.uncovered {
		t.Errorf("the writer did not record that a batch was replaced: %+v", mw.capture)
	}
}

// gateWriter blocks every Write until release is closed.
type gateWriter struct {
	release chan struct{}
	lockedBuffer
}

func (g *gateWriter) Write(p []byte) (int, error) {
	<-g.release
	return g.lockedBuffer.Write(p)
}

// The pipe holds at most maskPipeMax bytes the masker has not taken: past that a
// Write waits, so the pump stops reading the exec instead of buffering it.
func TestMaskPipe_AFullPipeStopsTheProducer(t *testing.T) {
	run := uuid.New()
	dst := &gateWriter{release: make(chan struct{})}
	p := newMaskPipe(&liveMaskWriter{reg: secretmask.NewRegistry(), runID: run, dst: dst})

	chunk := bytes.Repeat([]byte("z"), 32<<10)
	_, _ = p.Write(chunk) // taken by the drain goroutine, which parks in dst
	waitFor(t, "the first batch to be taken", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.queue) == 0
	})
	for range maskPipeMax / len(chunk) {
		_, _ = p.Write(chunk) // fills the queue to the bound
	}
	blocked := make(chan struct{})
	go func() {
		_, _ = p.Write(chunk)
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("a Write past maskPipeMax returned: the pipe buffers without bound")
	case <-time.After(200 * time.Millisecond):
	}
	p.mu.Lock()
	held := len(p.queue)
	p.mu.Unlock()
	if held > maskPipeMax {
		t.Fatalf("the pipe holds %d bytes, over maskPipeMax %d", held, maskPipeMax)
	}
	close(dst.release)
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the producer stayed blocked after the masker drained")
	}
	p.flush()
	if got, want := len(dst.String()), (maskPipeMax/len(chunk)+2)*len(chunk); got != want {
		t.Fatalf("the masker forwarded %d bytes, want %d", got, want)
	}
}
