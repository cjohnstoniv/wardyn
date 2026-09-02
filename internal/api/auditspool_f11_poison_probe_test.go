// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F11 PROBE — destination: internal/api/auditspool_f11_poison_probe_test.go
//
// Hypothesis H7: AuditSpool.Drain stops at the FIRST replay error and keeps
// that line at the head of the file (auditspool.go: `replayErr = err; break`,
// then the remainder is rewritten from `consumed`). That is right for a store
// that is DOWN (retry later) and wrong for a line the store will NEVER accept
// (a CHECK-constraint violation, a payload the column type rejects, a line a
// human edited): every event behind it is never replayed, forever, and the
// only signal is wardyn_audit_spool_lines never returning to 0.
//
// No Postgres needed. Expected result on feat/v0.7-profiles @ fa910735: RED —
// that is the finding, not a broken probe.
package api

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rejectingRecorder is fakeRecorder with one PERMANENTLY rejected action — the
// store is up (every other event lands), this one row is unacceptable.
type rejectingRecorder struct {
	fakeRecorder
	reject string
}

func (r *rejectingRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action == r.reject {
		return errors.New("ERROR: new row for relation \"audit_events\" violates check constraint (SQLSTATE 23514)")
	}
	return r.fakeRecorder.Record(ctx, ev)
}

func TestProbeF11_SpoolPoisonLineDoesNotWedgeLaterLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	for _, action := range []string{"good.one", "poison", "good.two"} {
		if err := sp.Append(newTestEvent(action)); err != nil {
			t.Fatalf("append %s: %v", action, err)
		}
	}

	rec := &rejectingRecorder{reject: "poison"}
	// Three StartDrain ticks' worth of attempts.
	var lastErr error
	for i := 0; i < 3; i++ {
		_, lastErr = sp.Drain(context.Background(), rec, 100)
	}
	if lastErr == nil {
		t.Errorf("Drain reported no error although one line can never be replayed")
	}
	if got := rec.count(); got != 2 {
		t.Fatalf("KNOWN GAP (F11 H7): after 3 drain ticks only %d of the 2 GOOD events reached the store; a permanently-rejected "+
			"spool line at the head of the file blocks every line behind it forever (spool lines left: %d). "+
			"docs/OPERATIONS.md promises the trail 'becomes complete again automatically'.", got, spoolLineCount(t, path))
	}
}
