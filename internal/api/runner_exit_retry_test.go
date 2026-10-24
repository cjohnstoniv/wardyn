// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/retrylog"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/google/uuid"
)

// A receipt's next try waits a doubling delay from its first failure; only refusals in a row count
// toward parking, so a transient error between them starts the count again.
func TestRunnerExitRetryWaitsDoublingAndCountsRefusalsInARow(t *testing.T) {
	var b runnerExitRetry
	key := runnerExitKey{uuid.New(), uuid.New()}
	now := time.Unix(1_700_000_000, 0)
	if !b.due(key, now) {
		t.Fatal("a receipt never tried is not due")
	}
	for try := 1; try <= 3; try++ {
		wait, refused := b.failed(key, store.ErrRunnerExecutionRefused, now)
		if wait != retrylog.Delay(try) || refused != try {
			t.Fatalf("try %d: wait %s refused %d, want %s and %d", try, wait, refused, retrylog.Delay(try), try)
		}
		if b.due(key, now.Add(wait-time.Second)) || !b.due(key, now.Add(wait)) {
			t.Fatalf("try %d: due before its %s wait, or not after it", try, wait)
		}
		now = now.Add(wait)
	}
	if wait, refused := b.failed(key, errors.New("runner temporarily disconnected"), now); wait != retrylog.Delay(4) || refused != 0 {
		t.Fatalf("a transient failure: wait %s refused %d, want %s and 0", wait, refused, retrylog.Delay(4))
	}
	if _, refused := b.failed(key, store.ErrRunnerExecutionRefused, now); refused != 1 {
		t.Fatalf("refusals after a transient failure = %d, want the count restarted", refused)
	}
	b.prune(now.Add(2*retrylog.Cap + time.Second))
	if !b.due(key, now.Add(2*retrylog.Cap+time.Second)) || len(b.receipts) != 0 {
		t.Fatal("a receipt no sweep tried for twice the cap was kept")
	}
}

// Fifty receipts failing alike in one sweep log one line; the next line, an interval later, counts the
// forty-nine left out. A different error is logged at once.
func TestRunnerExitRetryLogsAnIdenticalErrorOncePerInterval(t *testing.T) {
	logs := captureSlog(t)
	var b runnerExitRetry
	now := time.Unix(1_700_000_000, 0)
	for range 50 {
		b.warn(t.Context(), now, store.ErrRunnerExecutionRefused, "error", store.ErrRunnerExecutionRefused)
	}
	b.warn(t.Context(), now, errors.New("list runner exit work: connection reset"), "error", "other")
	b.warn(t.Context(), now.Add(retrylog.First), store.ErrRunnerExecutionRefused, "error", store.ErrRunnerExecutionRefused)
	got := logs.String()
	if n := strings.Count(got, "runner exit reconciliation will retry"); n != 3 {
		t.Fatalf("%d lines, want 3:\n%s", n, got)
	}
	if !strings.Contains(got, "repeated=0") || !strings.Contains(got, "repeated=49") {
		t.Errorf("the lines do not count what they left out:\n%s", got)
	}
}
