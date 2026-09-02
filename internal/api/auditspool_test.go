// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// TestAuditSpoolDownStoreIsNeverQuarantined is the counterweight to the poison
// probe, and the one that decides whether the quarantine is safe to ship. A
// store that is DOWN rejects the head line exactly the way a poison line does,
// so a rule that only counted rejections would move perfectly good events aside
// during a database restart — destroying, in the name of unblocking the queue,
// the events the C1 spool exists to preserve. Drain therefore quarantines only
// after the store has ACCEPTED a line from behind the suspect (Drain's poison
// probe), which a down store never does.
//
// Ten drain ticks, far past spoolPoisonAttempts: nothing is quarantined,
// nothing is dropped, and everything replays once the store heals.
func TestAuditSpoolDownStoreIsNeverQuarantined(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	for _, action := range []string{"one", "two", "three"} {
		if err := sp.Append(newTestEvent(action)); err != nil {
			t.Fatalf("append %s: %v", action, err)
		}
	}

	rec := &fakeRecorder{fail: true}
	for i := 0; i < 10; i++ {
		if n, err := sp.Drain(context.Background(), rec, 100); n != 0 || err == nil {
			t.Fatalf("tick %d: Drain = %d, %v; want 0 replayed and the store error", i, n, err)
		}
	}
	if got := sp.Quarantined(); got != 0 {
		t.Errorf("Quarantined = %d after 10 ticks against a DOWN store; want 0 — a store outage must never move an event aside", got)
	}
	if _, err := os.Stat(path + ".quarantine"); !os.IsNotExist(err) {
		t.Errorf("a quarantine file exists after a pure outage (stat err = %v); want none", err)
	}
	if got := spoolLineCount(t, path); got != 3 {
		t.Fatalf("spool holds %d lines after the outage, want all 3 still there", got)
	}

	rec.mu.Lock()
	rec.fail = false
	rec.mu.Unlock()
	if _, err := sp.Drain(context.Background(), rec, 100); err != nil {
		t.Fatalf("Drain after heal: %v", err)
	}
	if got := rec.count(); got != 3 {
		t.Errorf("replayed %d events after the store healed, want all 3", got)
	}
}

// TestAuditSpoolQuarantinedLineIsKeptOnDisk pins the other half of the poison
// fix: the blocked lines move on, and the rejected event is not DESTROYED to
// achieve it. It lands verbatim in the sidecar file — a valid JSONL spool an
// operator can move back once the cause is fixed — and the counter behind
// wardyn_audit_spool_quarantined_total says one event is missing from the
// queryable trail, which a spool that has drained back to 0 otherwise hides.
func TestAuditSpoolQuarantinedLineIsKeptOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	poison := newTestEvent("poison")
	for _, ev := range []types.AuditEvent{poison, newTestEvent("good")} {
		if err := sp.Append(ev); err != nil {
			t.Fatalf("append %s: %v", ev.Action, err)
		}
	}

	rec := &rejectingRecorder{reject: "poison"}
	for i := 0; i < spoolPoisonAttempts; i++ {
		if _, err := sp.Drain(context.Background(), rec, 100); err == nil {
			t.Fatalf("tick %d: Drain reported no error although one line is permanently rejected", i)
		}
	}
	if got := spoolLineCount(t, path); got != 0 {
		t.Errorf("spool still holds %d lines; the quarantine did not unblock it", got)
	}
	if got := sp.Quarantined(); got != 1 {
		t.Errorf("Quarantined = %d, want 1 — /metrics is the only place a drained-but-incomplete trail shows", got)
	}
	buf, err := os.ReadFile(path + ".quarantine")
	if err != nil {
		t.Fatalf("read quarantine file: %v", err)
	}
	var kept types.AuditEvent
	if err := json.Unmarshal(bytes.TrimRight(buf, "\n"), &kept); err != nil {
		t.Fatalf("quarantine file is not the verbatim JSONL line an operator can re-feed: %v (%q)", err, buf)
	}
	if kept.ID != poison.ID {
		t.Errorf("quarantined event id = %s, want the rejected event %s", kept.ID, poison.ID)
	}
}

// blockingRecorder blocks in Record until its ctx is done for ONE action and
// serves every other event normally — the store call that waits forever on the
// audit-chain advisory lock because an external session inserted into
// audit_events and left its transaction open (possible since migration 0056,
// which made the trigger take that lock) while the rest of the store is fine.
type blockingRecorder struct {
	fakeRecorder
	block   string
	entered chan struct{}
	once    sync.Once
}

func (r *blockingRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action != r.block {
		return r.fakeRecorder.Record(ctx, ev)
	}
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	return ctx.Err()
}

// TestAuditSpoolDrainIsBoundedAndDoesNotStrike pins both halves of the bound.
//
// BOUNDED: Drain holds a.mu across every Record, so a Record that never returns
// does not merely stall the drain — every request whose own audit write failed
// queues on Append behind it, and one idle psql transaction becomes a
// process-wide stall. The pass carries a deadline (spoolDrainDeadline, or the
// caller's own if it is shorter — which is the path this test drives, so the
// suite does not wait out the real 15s).
//
// AND NOT A STRIKE: a store that never ANSWERED has proved nothing about the
// line it was given, so a timeout must not count toward quarantine. The fixture
// makes that discriminating — the blocked line has a perfectly good line behind
// it, so if a timeout earned strikes, the third pass would promote the blocked
// line to suspect, replay the good one, take that as proof the store is up, and
// quarantine an event the store never rejected.
func TestAuditSpoolDrainIsBoundedAndDoesNotStrike(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	for _, action := range []string{"blocked", "good"} {
		if err := sp.Append(newTestEvent(action)); err != nil {
			t.Fatalf("append %s: %v", action, err)
		}
	}

	rec := &blockingRecorder{block: "blocked", entered: make(chan struct{})}
	for i := 0; i < spoolPoisonAttempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		done := make(chan error, 1)
		go func() { _, derr := sp.Drain(ctx, rec, 100); done <- derr }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("pass %d: a Drain whose store call never returned reported success", i)
			}
		case <-time.After(10 * time.Second):
			cancel()
			t.Fatal("Drain outlived its context by 10s; the spool lock is held for an unbounded time and Append is starved behind it")
		}
		cancel()
	}
	select {
	case <-rec.entered:
	case <-time.After(time.Second):
		t.Fatal("the blocking recorder was never reached; the fixture proves nothing")
	}

	if got := sp.Quarantined(); got != 0 {
		t.Errorf("Quarantined = %d after %d stalled passes; a store that never ANSWERED rejected nothing, and a good event behind the stalled one must not turn that into proof",
			got, spoolPoisonAttempts)
	}
	if _, err := os.Stat(path + ".quarantine"); !os.IsNotExist(err) {
		t.Errorf("a quarantine file exists after nothing but timeouts (stat err = %v); want none", err)
	}
	if got := spoolLineCount(t, path); got != 2 {
		t.Errorf("spool holds %d lines, want both events still there", got)
	}
}
