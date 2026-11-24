// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestAuditWriteTimedOutCountsABusyAppend(t *testing.T) {
	if !auditWriteTimedOut(fmt.Errorf("record: %w", store.ErrAuditAppendBusy)) {
		t.Error("a wrapped ErrAuditAppendBusy is a wait the store never answered, not a rejection")
	}
	if auditWriteTimedOut(fmt.Errorf("a plain failure")) {
		t.Error("a plain error is not a timeout")
	}
}

// busyHeadRecorder answers a wrapped ErrAuditAppendBusy for the head's id on its first busyCalls calls and accepts
// it after; every other id is accepted on every call.
type busyHeadRecorder struct {
	mu        sync.Mutex
	head      uuid.UUID
	busyCalls int
	headCalls int
	landed    []string
}

func (r *busyHeadRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ev.ID == r.head {
		r.headCalls++
		if r.headCalls <= r.busyCalls {
			return fmt.Errorf("record: %w", store.ErrAuditAppendBusy)
		}
	}
	r.landed = append(r.landed, ev.Action)
	return nil
}

// A line the store answered "busy" for, however many passes running, earns no strike: the line behind it, which the
// store does take, must not turn the busy answers into proof that the head is poison.
func TestAuditSpoolDrain_ABusyAppendEarnsNoStrikeAndQuarantinesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	head, second := newTestEvent("head"), newTestEvent("second")
	for _, ev := range []types.AuditEvent{head, second} {
		if err := sp.Append(ev); err != nil {
			t.Fatalf("append %s: %v", ev.Action, err)
		}
	}
	rec := &busyHeadRecorder{head: head.ID, busyCalls: 4}

	for pass := 1; pass <= 4; pass++ {
		_, _ = sp.Drain(context.Background(), rec, 100)
		if got := sp.Quarantined(); got != 0 {
			t.Fatalf("pass %d: Quarantined = %d; a busy append rejected nothing", pass, got)
		}
		if _, err := os.Stat(path + ".quarantine"); !os.IsNotExist(err) {
			t.Fatalf("pass %d: a quarantine file exists (stat err = %v)", pass, err)
		}
	}
	if _, err := sp.Drain(context.Background(), rec, 100); err != nil {
		t.Fatalf("the pass after the busy answers: %v", err)
	}
	if len(rec.landed) != 2 || rec.landed[0] != "head" || rec.landed[1] != "second" {
		t.Fatalf("landed %v, want head then second", rec.landed)
	}
	if got := spoolLineCount(t, path); got != 0 {
		t.Errorf("spool holds %d lines after both landed", got)
	}
}
