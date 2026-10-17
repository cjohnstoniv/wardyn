// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// A drop counted in a batch is kept when a later record of the same batch fails to write: its caller was
// told ErrFull, so replay must still count it.
func TestADropInAFailedBatchIsStillCounted(t *testing.T) {
	ctx := context.Background()
	s, dir, id := testSpool(t)
	// The first record's file name is taken by a directory, so its write fails at the rename.
	if err := os.MkdirAll(filepath.Join(dir, recordName(1), "x"), 0700); err != nil {
		t.Fatal(err)
	}
	big := decision(id)
	big.Request.Host = strings.Repeat("a", MaxRecordBytes)

	s.commitMu.Lock() // a commit in progress, so both appends land in one batch, the oversized one first
	errs := make([]chan error, 2)
	for i, d := range []egress.DecisionLog{big, decision(id)} {
		errs[i] = make(chan error, 1)
		go func() { errs[i] <- s.Append(ctx, d) }()
		waitQueued(t, s, i+1)
	}
	s.commitMu.Unlock()
	if err := <-errs[0]; !errors.Is(err, ErrFull) {
		t.Fatalf("oversized append = %v, want ErrFull", err)
	}
	if err := <-errs[1]; err == nil {
		t.Fatal("the append whose write failed succeeded")
	}
	if depth, _, drops, err := s.Stats(ctx); err != nil || depth != 0 || drops != 1 {
		t.Fatalf("after the failed batch: depth %d drops %d err %v, want depth 0 and the one drop kept", depth, drops, err)
	}
}

func waitQueued(t *testing.T, s *Spool, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		s.qmu.Lock()
		queued := len(s.queue)
		s.qmu.Unlock()
		if queued == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d appenders queued", queued, n)
		}
	}
}
