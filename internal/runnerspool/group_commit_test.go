// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"testing"
	"time"
)

// Appenders that queue while another commit holds the spool share the next manifest write, and each
// returns only after the manifest naming its record is written.
func TestAppendersQueuedBehindACommitShareOneManifestWrite(t *testing.T) {
	ctx := context.Background()
	s, _, id := testSpool(t)
	s.mu.Lock()
	before := s.manifestWrites
	s.mu.Unlock()

	const n = 8
	s.commitMu.Lock() // a commit in progress
	errs := make(chan error, n)
	for range n {
		go func() { errs <- s.Append(ctx, decision(id)) }()
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		s.qmu.Lock()
		queued := len(s.queue)
		s.qmu.Unlock()
		if queued == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d appenders queued", queued, n)
		}
	}
	s.commitMu.Unlock()
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	writes := s.manifestWrites - before
	s.mu.Unlock()
	if writes != 1 {
		t.Fatalf("%d queued appends took %d manifest writes, want 1", n, writes)
	}
	var seq int64
	if err := s.Replay(ctx, each(func(r Record) error {
		seq++
		if r.Seq != seq {
			t.Fatalf("sequence %d after %d", r.Seq, seq-1)
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if seq != n {
		t.Fatalf("replayed %d records, want %d", seq, n)
	}
}
