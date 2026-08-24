// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeRecorder records events into a slice; when fail is set every Record errors,
// modelling a down store. It is the drain's replay target.
type fakeRecorder struct {
	mu   sync.Mutex
	fail bool
	got  []types.AuditEvent
}

func (r *fakeRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("store down")
	}
	r.got = append(r.got, ev)
	return nil
}

func (r *fakeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func spoolLineCount(t *testing.T, path string) int {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	buf = bytes.TrimRight(buf, "\n")
	if len(buf) == 0 {
		return 0
	}
	return bytes.Count(buf, []byte{'\n'}) + 1
}

func newTestEvent(action string) types.AuditEvent {
	return types.AuditEvent{
		ID:        uuid.New(),
		ActorType: types.ActorHuman,
		Actor:     "tester",
		Action:    action,
		Outcome:   "success",
	}
}

// TestNewAuditSpool_CreatesMissingParentDir pins the W28-S1-2 fix: the flag
// default (cmd/wardynd/boot_flags.go) is the RELATIVE "./data/audit-spool.jsonl",
// and the chart's own defaults point it at a directory nothing has created yet
// (an emptyDir or a fresh PVC). Before the MkdirAll in NewAuditSpool, opening a
// path whose parent directory does not exist failed outright — this fails on
// that base and passes once the parent is created for it.
func TestNewAuditSpool_CreatesMissingParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "audit", "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool with missing parent dir: %v", err)
	}
	if err := sp.Append(newTestEvent("run.create")); err != nil {
		t.Fatalf("Append after self-healed mkdir: %v", err)
	}
	if lc := spoolLineCount(t, path); lc != 1 {
		t.Fatalf("spool after Append: %d lines, want 1", lc)
	}
}

// TestAuditSpoolDrainReplaysAfterHeal is the counterfactual: without a drain
// the spool is a write-only sink and never empties. Spool N events while the store
// is down (drain lands nothing, file intact), heal the store, and assert all N
// events reach the recorder and the spool empties.
func TestAuditSpoolDrainReplaysAfterHeal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	const n = 7
	for i := 0; i < n; i++ {
		if err := sp.Append(newTestEvent("credential.mint")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	rec := &fakeRecorder{fail: true}
	// Store still down: drain replays nothing and leaves every line on disk.
	got, err := sp.Drain(context.Background(), rec, 100)
	if err == nil {
		t.Fatalf("Drain: want replay error while store down, got nil")
	}
	if got != 0 {
		t.Fatalf("Drain while down: replayed %d, want 0", got)
	}
	if lc := spoolLineCount(t, path); lc != n {
		t.Fatalf("spool after failed drain: %d lines, want %d", lc, n)
	}

	// Heal the store: drain replays every event and empties the spool.
	rec.fail = false
	got, err = sp.Drain(context.Background(), rec, 100)
	if err != nil {
		t.Fatalf("Drain after heal: %v", err)
	}
	if got != n {
		t.Fatalf("Drain after heal: replayed %d, want %d", got, n)
	}
	if rec.count() != n {
		t.Fatalf("recorder saw %d events, want %d", rec.count(), n)
	}
	if lc := spoolLineCount(t, path); lc != 0 {
		t.Fatalf("spool not empty after drain: %d lines", lc)
	}

	// A drain of an already-empty spool is a clean no-op.
	got, err = sp.Drain(context.Background(), rec, 100)
	if err != nil || got != 0 {
		t.Fatalf("Drain of empty spool: got=%d err=%v, want 0,nil", got, err)
	}
}

// TestAuditSpoolDrainBounded confirms the batch bound: a backlog larger than the
// batch drains in confirmed slices, removing only what it replayed each call.
func TestAuditSpoolDrainBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := sp.Append(newTestEvent("egress.deny")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	rec := &fakeRecorder{}

	got, err := sp.Drain(context.Background(), rec, 2)
	if err != nil || got != 2 {
		t.Fatalf("first drain: got=%d err=%v, want 2,nil", got, err)
	}
	if lc := spoolLineCount(t, path); lc != 3 {
		t.Fatalf("after first drain: %d lines, want 3", lc)
	}

	got, _ = sp.Drain(context.Background(), rec, 2)
	if got != 2 {
		t.Fatalf("second drain: replayed %d, want 2", got)
	}
	got, _ = sp.Drain(context.Background(), rec, 2)
	if got != 1 {
		t.Fatalf("third drain: replayed %d, want 1", got)
	}
	if rec.count() != 5 {
		t.Fatalf("recorder saw %d events, want 5", rec.count())
	}
	if lc := spoolLineCount(t, path); lc != 0 {
		t.Fatalf("spool not empty: %d lines", lc)
	}
}

// TestAuditSpoolDrainReopensAfterTrim pins the crash-safe trim: Drain rewrites the
// remainder via a temp file + atomic rename, which replaces the spool inode, so it
// MUST reopen a.f on the new file. If it does not, an Append after a partial drain
// writes to the renamed-away (unlinked) inode and is silently lost — this test
// fails (the appended event never drains and a stale .tmp may linger).
func TestAuditSpoolDrainReopensAfterTrim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := sp.Append(newTestEvent("run.kill")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	rec := &fakeRecorder{}

	// Partial drain (batch 2) triggers the rename+reopen with a 2-line remainder.
	if got, err := sp.Drain(context.Background(), rec, 2); err != nil || got != 2 {
		t.Fatalf("partial drain: got=%d err=%v, want 2,nil", got, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("stale temp spool left behind: err=%v", err)
	}

	// Append onto the reopened fd, then drain the rest: the new event must land.
	if err := sp.Append(newTestEvent("credential.mint")); err != nil {
		t.Fatalf("Append after trim: %v", err)
	}
	if got, err := sp.Drain(context.Background(), rec, 100); err != nil || got != 3 {
		t.Fatalf("final drain: got=%d err=%v, want 3,nil", got, err)
	}
	if rec.count() != 5 {
		t.Fatalf("recorder saw %d events, want 5 (post-trim Append lost?)", rec.count())
	}
	if lc := spoolLineCount(t, path); lc != 0 {
		t.Fatalf("spool not empty after drain: %d lines", lc)
	}
}

// TestAuditSpoolAppendRecoversTornTail is the D30 regression: an ENOSPC episode
// (or a partial Write that landed bytes then errored) leaves a newline-less
// fragment at EOF. The NEXT Append must not concatenate onto it — otherwise
// Drain reads `fragment{good event}` as one line, fails to unmarshal it, and
// drops BOTH, destroying a good event whose primary-store write had already
// failed. The separator confines the loss to the torn fragment; the good event
// survives and is replayed, and the torn line is counted.
//
// RED before the fix: recovered == 0 (the good event was swallowed with the
// fragment) and TornDrops covers a merged line. GREEN after: recovered == 1 and
// TornDrops == 1.
func TestAuditSpoolAppendRecoversTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	// Simulate a torn tail: a partial JSON fragment with NO trailing newline,
	// exactly what an ENOSPC leaves behind mid-line.
	if err := os.WriteFile(path, []byte(`{"id":"deadbeef","action":"cred`), 0o600); err != nil {
		t.Fatalf("seed torn tail: %v", err)
	}

	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	// The good event whose durable write failed, now spooled after the torn tail.
	if err := sp.Append(newTestEvent("credential.mint")); err != nil {
		t.Fatalf("Append onto torn tail: %v", err)
	}

	rec := &fakeRecorder{}
	got, err := sp.Drain(context.Background(), rec, 100)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got != 1 {
		t.Fatalf("Drain recovered %d events, want 1 (the good event must survive the torn tail)", got)
	}
	if rec.count() != 1 {
		t.Fatalf("recorder saw %d events, want 1", rec.count())
	}
	if td := sp.TornDrops(); td != 1 {
		t.Fatalf("TornDrops = %d, want 1 (the torn fragment, and only it, is dropped)", td)
	}
	if lc := spoolLineCount(t, path); lc != 0 {
		t.Fatalf("spool not empty after drain: %d lines", lc)
	}
}
