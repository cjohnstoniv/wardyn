// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordStoreDrain lands a drained audit event where the record fake's QueryAuditEvents reads it.
type recordStoreDrain struct{ s *recordStore }

func (d recordStoreDrain) Record(_ context.Context, ev types.AuditEvent) error {
	d.s.events = append(d.s.events, ev)
	return nil
}

// spoolingReadStore is a recordStore whose first audit read runs during once, under the read: a store
// write that fails while a capture reads.
type spoolingReadStore struct {
	*recordStore
	during func()
}

func (s *spoolingReadStore) QueryAuditEvents(ctx context.Context, id uuid.UUID, n int) ([]types.AuditEvent, error) {
	if during := s.during; during != nil {
		s.during = nil
		during()
	}
	return s.recordStore.QueryAuditEvents(ctx, id, n)
}

// A deny whose store write failed waits in the audit spool. A capture taken then would miss it and stamp
// the confined replay clean, so the entry stays recording until the spool has drained, and the capture
// after the drain counts the deny. A deny spooled while the capture's read is under way is missed by that
// read as well, so that capture is not taken either.
func TestReconcileRecordRun_WaitsForTheAuditSpoolToDrain(t *testing.T) {
	h := newHarness(t)
	runID, wsID := uuid.New(), uuid.New()
	ws := recordingWorkspace(wsID, runID, "verify")
	ws.RecordResults = mustJSON(map[string]RecordTaskResult{
		"verify": {RunID: runID, Mode: "auto", Confined: true, Status: recordStatusRecording},
	})
	fake := &recordStore{
		run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunCompleted},
		importStateFake: importStateFake{ws: ws},
		events:          []types.AuditEvent{egressAllowEvent(runID, "pypi.org")},
	}
	spool, err := NewAuditSpool(filepath.Join(t.TempDir(), "audit-spool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// The primary insert of this deny fails during the first capture's read, so it is spooled then (spoolingRecorder).
	deny := types.AuditEvent{RunID: &runID, Action: "egress.deny", Outcome: "denied", Target: "example.com",
		Data: mustJSON(map[string]any{"host": "example.com", "method": "GET", "rule_source": "approval:denied"})}
	st := &spoolingReadStore{recordStore: fake, during: func() {
		if err := spool.Append(deny); err != nil {
			t.Fatal(err)
		}
	}}
	cfg := baseTestConfig(h, st)
	cfg.AuditSpool = spool
	srv := New(cfg)

	srv.reconcileRecordRun(context.Background(), runID)
	if fake.saved != nil {
		t.Fatalf("finalized over a deny spooled during its read: %s", fake.saved)
	}
	srv.reconcileRecordRun(context.Background(), runID)
	if fake.saved != nil {
		t.Fatalf("finalized with the deny still in the spool: %s", fake.saved)
	}

	if n, err := spool.Drain(context.Background(), recordStoreDrain{fake}, 10); err != nil || n != 1 {
		t.Fatalf("drain = %d, %v; want the deny landed", n, err)
	}
	srv.reconcileRecordRun(context.Background(), runID)
	res := fake.savedResult(t, "verify")
	if res.Status != recordStatusRecorded || res.Clean == nil || *res.Clean || res.Caught != 1 {
		t.Fatalf("after the drain: status %q, clean %v, caught %d; want recorded, not clean, 1 caught", res.Status, res.Clean, res.Caught)
	}
}
