// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSweepTerminalSandboxes_UnreadableFolderRecordRowIsWrittenOnce: the sweep writes an unread folder record's
// sandbox.sweep row once, not on every pass, and again only after the record has read, the sandbox has been
// stopped, or the sandbox has gone. The sandbox is kept with its proxy stopped on every pass that keeps it.
func TestSweepTerminalSandboxes_UnreadableFolderRecordRowIsWrittenOnce(t *testing.T) {
	run := sweepRun(types.RunFailed, "sbx-unread")
	fake := &keptSweepStore{sweepStore: &sweepStore{runs: []types.AgentRun{run}}, unreadable: run.ID}
	srv, rr, h := newKeptSweep(t, fake)
	key := teardownRowKey{srv, run.ID, sandboxSweepAction}
	pass := func(step string, rows int, held bool) {
		t.Helper()
		if _, err := srv.SweepTerminalSandboxes(context.Background()); err != nil {
			t.Fatalf("%s: sweep: %v", step, err)
		}
		n := 0
		for _, ev := range runRows(h.audit.snapshot(), run.ID) {
			if ev.Action == sandboxSweepAction && ev.Outcome == "failure" && strings.Contains(string(ev.Data), `"folder_record_error"`) {
				n++
			}
		}
		if _, ok := folderRecordRowsWritten.Load(key); n != rows || ok != held {
			t.Fatalf("%s: %d folder_record_error rows, entry held %v; want %d, %v", step, n, ok, rows, held)
		}
	}
	pass("first unread pass", 1, true)
	pass("second unread pass", 1, true)
	fake.unreadable, fake.kept = uuid.Nil, run.ID
	pass("the record reads", 1, false)
	fake.unreadable, fake.kept = run.ID, uuid.Nil
	pass("unread again", 2, true)
	fake.runs[0].MachineKind = types.MachineLinuxVM
	pass("a sandbox that cannot be kept is stopped", 2, false)
	if len(rr.stopped) != 1 {
		t.Fatalf("StopSandbox %v, want the one stop of the virtual machine pass", rr.stopped)
	}
	fake.runs[0].MachineKind = ""
	pass("unread after the stop", 3, true)
	srv.cfg.Runner = &sweepGoneRunner{fakeRunner: &fakeRunner{}}
	pass("the sandbox is gone", 3, false)
}
