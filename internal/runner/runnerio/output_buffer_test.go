// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

func outputFixture(t *testing.T) *outputBuffer {
	t.Helper()
	b, err := newOutputBuffer(filepath.Join(t.TempDir(), "run"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOutputBufferRestartAndAcknowledgedRange(t *testing.T) {
	b := outputFixture(t)
	want := bytes.Repeat([]byte("abcdefghijklmnopqrstuvwxyz"), 4000)
	if n, err := b.Write(want); n != len(want) || err != nil {
		t.Fatalf("write=%d,%v", n, err)
	}
	if err := b.ack(73); err != nil {
		t.Fatal(err)
	}
	beforeEOF, err := newOutputBuffer(b.dir)
	if err != nil || beforeEOF.state.Ack != 73 {
		t.Fatalf("ACK was not durable before EOF: state=%+v err=%v", beforeEOF, err)
	}
	b.EndDrain(nil)
	recovered, err := newOutputBuffer(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.state.Ack != 73 {
		t.Fatalf("ack=%d", recovered.state.Ack)
	}
	var got []byte
	scratch := make([]byte, 12347)
	for {
		n, err := recovered.read(t.Context(), int64(len(got)), scratch)
		got = append(got, scratch[:n]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal("recovered bytes changed or reordered")
	}
	if err := recovered.ack(int64(len(want)) + 1); !errors.Is(err, errOutputOffset) {
		t.Fatalf("ahead ack=%v", err)
	}
}

func TestOutputBufferBoundBackpressureAndGap(t *testing.T) {
	synctest.Test(t, outputBackpressureAndGap)
}

func outputBackpressureAndGap(t *testing.T) {
	b := outputFixture(t)
	want := bytes.Repeat([]byte("x"), outputLimit)
	if _, err := b.Write(want); err != nil {
		t.Fatal(err)
	}
	started, done := make(chan struct{}), make(chan error, 1)
	go func() { close(started); _, err := b.Write([]byte("last")); done <- err }()
	<-started
	synctest.Wait()
	select {
	case err := <-done:
		t.Fatalf("unacknowledged full buffer did not block: %v", err)
	default:
	}
	// No space is reclaimed until an explicit consumer ACK; a read alone is insufficient.
	scratch := make([]byte, outputChunk)
	if n, err := b.read(t.Context(), 0, scratch); n != len(scratch) || err != nil {
		t.Fatalf("read=%d,%v", n, err)
	}
	b.mu.Lock()
	if b.state.End != outputLimit {
		t.Fatal("unacknowledged data evicted")
	}
	b.mu.Unlock()
	if err := b.ack(outputChunk); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ACK did not release writer")
	}
	if _, err := b.read(t.Context(), 0, scratch); !errors.Is(err, runner.ErrOutputUnrecoverable) {
		t.Fatalf("old offset=%v", err)
	}
	if n, err := b.read(t.Context(), outputLimit, scratch); err != nil || string(scratch[:n]) != "last" {
		t.Fatalf("tail=%q,%v", scratch[:n], err)
	}
	var disk int64
	files, err := os.ReadDir(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".bin" {
			info, err := f.Info()
			if err != nil {
				t.Fatal(err)
			}
			disk += info.Size()
		}
	}
	if disk > outputLimit {
		t.Fatalf("output on disk=%d", disk)
	}
	b.Close()
}

func TestOutputBufferCrashDiscardsOnlyUncommittedTail(t *testing.T) {
	b := outputFixture(t)
	if _, err := b.Write([]byte("committed")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(b.dir, partName(0)), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte("uncommitted")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	orphan := filepath.Join(b.dir, partName(100))
	if err := os.WriteFile(orphan, []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := newOutputBuffer(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	scratch := make([]byte, 100)
	n, err := recovered.read(t.Context(), 0, scratch)
	if err != nil || string(scratch[:n]) != "committed" {
		t.Fatalf("read=%q,%v", scratch[:n], err)
	}
	if _, err = recovered.read(t.Context(), int64(n), scratch); !errors.Is(err, runner.ErrOutputUnrecoverable) {
		t.Fatalf("interrupted drain=%v", err)
	}
	if _, err = os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan=%v", err)
	}
}

func TestOutputBufferMissingCommittedBytesRefused(t *testing.T) {
	b := outputFixture(t)
	if _, err := b.Write([]byte("committed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(b.dir, partName(0)), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := newOutputBuffer(b.dir); err == nil {
		t.Fatal("missing committed bytes accepted")
	}
}

func TestOutputBufferReadCancellationAndFailure(t *testing.T) {
	b := outputFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := b.read(ctx, 0, make([]byte, 8)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read=%v", err)
	}
	b.EndDrain(errors.New("source disconnected"))
	if _, err := b.read(t.Context(), 0, make([]byte, 8)); err == nil || err.Error() != "source disconnected" {
		t.Fatalf("failure=%v", err)
	}
}

func TestOutputBufferMissingStatePreservesExistingData(t *testing.T) {
	b := outputFixture(t)
	if _, err := b.Write([]byte("committed")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(b.dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := newOutputBuffer(b.dir); err == nil {
		t.Fatal("accepted missing state")
	}
	data, err := os.ReadFile(filepath.Join(b.dir, partName(0)))
	if err != nil || string(data) != "committed" {
		t.Fatal("destroyed existing output")
	}
}

func TestOutputBufferFailedAckCannotBecomeSuccessOnRetry(t *testing.T) {
	b := outputFixture(t)
	if _, err := b.Write([]byte("retained")); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(b.dir, "state.json")
	backup := filepath.Join(t.TempDir(), "state.json")
	if err := os.Rename(state, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := b.ack(4); err == nil {
			t.Fatalf("failed persistence reported ACK success on attempt%d", i)
		}
	}
	if _, err := b.Write([]byte("x")); err == nil {
		t.Fatal("output continued after persistence failure")
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, state); err != nil {
		t.Fatal(err)
	}
	recovered, err := newOutputBuffer(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.state.Ack != 0 {
		t.Fatalf("restored uncommitted ACK=%d", recovered.state.Ack)
	}
	var data [8]byte
	n, err := recovered.read(t.Context(), 0, data[:])
	if err != nil || string(data[:n]) != "retained" {
		t.Fatalf("committed bytes=%q err=%v", data[:n], err)
	}
}
