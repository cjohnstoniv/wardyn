// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// Two replicas finalise one run: the launcher's incomplete row commits first, then the complete row of the replica
// that took the run over replaces it, and only that replica audits the repair, as a success.
func TestPeerAdopterCompleteRowReplacesLauncherPartial_PG(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run)
	st := store.NewPG(l.pool)
	if err := st.InsertPendingRunOutput(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	writeExecOutput(t, a.srv.openExecOutput(run, false), "part\n")
	writeExecOutput(t, b.srv.openExecOutput(run, false), "part\nand the rest\n")
	ta, tb := a.srv.tailFor(run.ID), b.srv.tailFor(run.ID)
	if ta == nil || tb == nil {
		t.Fatal("each replica should hold a tail for the run")
	}

	partial := store.RunOutput{RunID: run.ID, Output: []byte("part\n"), Incomplete: true, Source: "stdout", MaskScope: "run"}
	if got := a.srv.tryPersistOutput(t.Context(), ta, st, &partial); got != persistDone {
		t.Fatalf("A's incomplete row: outcome %v, want persistDone", got)
	}
	if n := replacedAudits(t, l, run.ID); n != 0 {
		t.Fatalf("A's write over the pending row audited %d replacements", n)
	}
	full := store.RunOutput{RunID: run.ID, Output: []byte("part\nand the rest\n"), Source: "stdout", MaskScope: "run"}
	if got := b.srv.tryPersistOutput(t.Context(), tb, st, &full); got != persistDone {
		t.Fatalf("B's complete row: outcome %v, want persistDone", got)
	}
	row, found, err := st.GetRunOutput(t.Context(), run.ID)
	if err != nil || !found || string(row.Output) != "part\nand the rest\n" || row.Incomplete {
		t.Fatalf("stored row = %q incomplete=%v found=%v err=%v, want B's complete bytes", row.Output, row.Incomplete, found, err)
	}
	if n := replacedAudits(t, l, run.ID); n != 1 {
		t.Fatalf("%d run.output.finalize rows with replaced_incomplete, want B's one: %s", n, auditDump(l.rec.snapshot(), run.ID))
	}
}

// replacedAudits counts the run's run.output.finalize rows that report a replaced incomplete row, each checked for
// outcome success and step replace.
func replacedAudits(t *testing.T, l *maskLab, runID uuid.UUID) int {
	t.Helper()
	n := 0
	for _, ev := range l.rec.snapshot() {
		if ev.Action != "run.output.finalize" || ev.RunID == nil || *ev.RunID != runID {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["replaced_incomplete"] != true {
			continue
		}
		if ev.Outcome != "success" || data["step"] != "replace" {
			t.Errorf("replacement audit outcome %q step %v, want success and replace", ev.Outcome, data["step"])
		}
		n++
	}
	return n
}
