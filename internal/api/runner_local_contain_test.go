// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Each local containment a STATE page first reports is one runner.local_contain.record row, actor the runner,
// runner-asserted.
func TestRecordLocalContainEvents_OneRowPerRun(t *testing.T) {
	h := newHarness(t)
	runner, a, b := uuid.New(), uuid.New(), uuid.New()
	at := time.Now().UTC().Truncate(time.Second)
	h.srv.recordLocalContainEvents(t.Context(), []store.LocalContainEvent{
		{RunID: a, RunnerID: runner, ContainedAt: at, Scope: "run", OperatorUID: 501},
		{RunID: b, RunnerID: runner, ContainedAt: at, Scope: "all", OperatorUID: 0},
	})
	got := map[uuid.UUID]types.AuditEvent{}
	for _, ev := range h.audit.snapshot() {
		if ev.Action == auditRunnerLocalContain {
			got[*ev.RunID] = ev
		}
	}
	if len(got) != 2 {
		t.Fatalf("runner.local_contain.record rows = %d, want 2", len(got))
	}
	ev := got[a]
	if ev.ActorType != types.ActorSystem || ev.Actor != "runner:"+runner.String() || ev.Target != a.String() || ev.Outcome != "success" {
		t.Fatalf("row = %+v", ev)
	}
	var d map[string]any
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d["scope"] != "run" || d["operator_uid"] != float64(501) || d["evidence_source"] != string(types.RunEvidenceRunnerAsserted) || d["contained_at"] != at.Format(time.RFC3339) {
		t.Fatalf("row data = %v", d)
	}
	h.srv.recordLocalContainEvents(t.Context(), nil)
	if n := len(h.audit.snapshot()); n != 2 {
		t.Fatalf("rows after an empty page = %d, want 2", n)
	}
}

// A run on no runner, or a server whose store is not Postgres, is never refused as locally contained.
func TestReviveLocalContained_NoRunnerOrStoreIsNotRefused(t *testing.T) {
	h := newHarness(t)
	if rerr := h.srv.reviveLocalContained(t.Context(), types.AgentRun{ID: uuid.New()}); rerr != nil {
		t.Fatalf("a run on no runner = %+v, want nil", rerr)
	}
	rn := uuid.New()
	if rerr := h.srv.reviveLocalContained(t.Context(), types.AgentRun{ID: uuid.New(), RunnerID: &rn}); rerr != nil {
		t.Fatalf("a runner run on a non-Postgres store = %+v, want nil", rerr)
	}
}
