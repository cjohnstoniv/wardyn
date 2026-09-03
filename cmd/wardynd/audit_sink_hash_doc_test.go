// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// readDoc returns a repo document with markdown's hard wrapping collapsed, so a
// claim that reads as one sentence to a human is one string to Contains.
func readDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return strings.Join(strings.Fields(string(b)), " ")
}

// TestAuditSinkHashClaimIsQualified pins the docs' off-box tamper-evidence claim
// to the two code facts that bound it.
//
// The chain hashes exist only because store.InsertAuditEvent fills them by
// RETURNING; fanoutRecorder.Record emits to the sinks whether that write
// succeeded or not, and the spool drain replays into the RAW store recorder,
// which has no fanout. So an event written during a Postgres outage reaches a
// SIEM once, unchained, and never again — while OPERATIONS.md and
// AUDIT-ACTIONS.md said, without qualification, that EVERY event on a sink
// stream carries its prev_hash/row_hash and that this is "the control". An
// operator sizing their SIEM detection against that sentence would not know
// their head-hash series has a hole exactly where the store was unavailable.
//
// Both halves are asserted here: if the code is ever changed so the drain does
// re-stream (making the unqualified claim true), the first half fails and the
// doc should be re-widened deliberately; if the doc drifts back to the
// unqualified promise, the second half fails.
func TestAuditSinkHashClaimIsQualified(t *testing.T) {
	dir := t.TempDir()
	sinkPath := filepath.Join(dir, "audit.log")
	cfg, err := json.Marshal(map[string]any{"file": map[string]any{"path": sinkPath}})
	if err != nil {
		t.Fatalf("marshal sink config: %v", err)
	}
	// A nil pool is enough: nothing here writes to Postgres, and the drain
	// recorder's identity is decided at wiring time.
	_, fan, _, drainRec, err := buildAuditChain(context.Background(), string(cfg),
		filepath.Join(dir, "spool.jsonl"), "", nil, nil)
	if err != nil {
		t.Fatalf("buildAuditChain: %v", err)
	}
	if fan == nil {
		t.Fatal("no fanout built from a file-sink config — the rest of this test would be vacuous")
	}

	// (1) The drain replays into the raw store recorder, so a spooled event is
	// never streamed a second time, chained or otherwise.
	if _, isFanout := drainRec.(fanoutRecorder); isFanout {
		t.Error("the spool drain now records through the fanout — spooled events DO reach the sinks on replay, " +
			"so OPERATIONS.md's qualified claim understates the guarantee; re-widen the doc deliberately")
	}
	if _, isRaw := drainRec.(store.Recorder); !isRaw {
		t.Errorf("drain recorder is %T, want store.Recorder — this guard's premise (the drain bypasses the sinks) no longer holds", drainRec)
	}

	// (2) What a sink actually receives for an event whose store write failed:
	// PrevHash/RowHash are `omitempty`, so they are absent, not empty strings.
	if err := fan.Emit(context.Background(), types.AuditEvent{Action: "test.unchained", Outcome: "ok"}); err != nil {
		t.Fatalf("fanout emit: %v", err)
	}
	line, err := os.ReadFile(sinkPath)
	if err != nil {
		t.Fatalf("read sink file: %v", err)
	}
	for _, field := range []string{"prev_hash", "row_hash"} {
		if strings.Contains(string(line), field) {
			t.Errorf("a sink event with no store write carries %q (%s) — the docs' outage caveat would be wrong", field, line)
		}
	}

	// (3) The docs must carry the qualifier, and must not carry the old
	// unqualified promise.
	ops := readDoc(t, "docs/OPERATIONS.md")
	for _, want := range []string{
		"event **whose Postgres write succeeded** carries its `prev_hash`/`row_hash`",
		"**What the drain does not restore: the off-box hash series.**",
	} {
		if !strings.Contains(ops, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("docs/OPERATIONS.md no longer states the outage caveat: missing %q", want)
		}
	}
	if strings.Contains(ops, "every event on an audit sink stream (`WARDYN_AUDIT_SINKS`) carries its `prev_hash`/`row_hash`") {
		t.Error("docs/OPERATIONS.md is back to the unqualified claim: a spooled event fans out unchained and is never re-streamed")
	}
	actions := readDoc(t, "docs/AUDIT-ACTIONS.md")
	if strings.Contains(actions, "(webhook/syslog/file), which is what puts a head hash in your SIEM") {
		t.Error("docs/AUDIT-ACTIONS.md still promises the sink stream always carries the hashes")
	}
	if !strings.Contains(actions, "is not re-streamed when the spool drains") {
		t.Error("docs/AUDIT-ACTIONS.md never says a spooled event is not re-streamed on drain")
	}
}
