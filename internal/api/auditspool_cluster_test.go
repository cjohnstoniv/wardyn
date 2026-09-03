// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// PINS for the four review findings against the audit spool. Three of them
// (quadratic drain, /metrics blocking, the post-rename fd) share one structural
// cause — Drain held a.mu across whole-file I/O on every pass — but they fail in
// three different ways and are pinned separately, because a throughput
// assertion cannot see a lost event and a lost-event assertion cannot see a
// stalled scrape.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// drainAll runs Drain to completion the way StartDrain's inner loop does, and
// returns how many bytes the passes WROTE. A pass writes only when it compacts,
// and a compaction writes exactly the surviving bytes — which is the file's new
// size — so a size that fell across a pass is that pass's write volume, and a
// size that did not is a pass that wrote nothing.
func drainAll(t *testing.T, sp *AuditSpool, rec *fakeRecorder, batch int) (passes int, written int64) {
	t.Helper()
	ctx := context.Background()
	for {
		before := spoolSize(t, sp.path)
		n, err := sp.Drain(ctx, rec, batch)
		if err != nil {
			t.Fatalf("drain pass %d: %v", passes, err)
		}
		passes++
		if after := spoolSize(t, sp.path); after < before {
			written += after
		}
		if n < batch {
			return passes, written
		}
	}
}

func spoolSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat spool: %v", err)
	}
	return fi.Size()
}

// TestAuditSpoolDrainWriteVolumeIsLinearInBacklog pins the cost of the recovery
// the spool exists to perform. Drain used to re-read the whole file and rewrite
// and fsync every surviving line on EVERY pass while replaying only `batch`
// events, so clearing a backlog of N cost O(N^2/batch): measured 159x write
// amplification at 64,000 spooled events, growing 4x per doubling, with the
// fsyncs landing on the same volume as the database that has just come back.
//
// The assertion is on the SHAPE, not on a wall clock: the bytes a full drain
// writes must stay within a small constant factor of the backlog, and that
// factor must not grow with the backlog. Compacting only once the consumed
// prefix is at least as large as the remainder halves the file each time, so
// the writes sum to at most 2N.
func TestAuditSpoolDrainWriteVolumeIsLinearInBacklog(t *testing.T) {
	const batch = 200
	small := spoolDrainAmplification(t, 2000, batch)
	large := spoolDrainAmplification(t, 32000, batch)
	t.Logf("write amplification: N=2000 -> %.2fx, N=32000 -> %.2fx", small, large)

	// 2N is the bound the halving rule gives; 4 leaves room for the final
	// truncate and for the last partial pass without admitting a growth trend.
	for _, c := range []struct {
		n   int
		amp float64
	}{{2000, small}, {32000, large}} {
		if c.amp > 4 {
			t.Errorf("draining %d spooled events wrote %.1fx the backlog; a full drain must cost O(N), "+
				"not O(N^2/batch) — this is the quadratic rewrite that made recovering an audit backlog "+
				"fsync hundreds of times its own size onto the volume the database is recovering on", c.n, c.amp)
		}
	}
	// The real defect is the TREND: at 16x the backlog the old code amplified
	// 16x harder. A constant-factor cost must not.
	if large > small*2 {
		t.Errorf("write amplification GREW with the backlog (%.2fx at 2000 -> %.2fx at 32000); "+
			"the cost of a drain must not scale with how long the outage was", small, large)
	}
}

func spoolDrainAmplification(t *testing.T, n, batch int) float64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	// The backlog is written in one go rather than through N Appends: Append
	// fsyncs per event by design, and paying tens of thousands of fsyncs to set
	// the test up measures the wrong thing (and takes a minute). The file is
	// byte-identical to what those Appends produce.
	var buf bytes.Buffer
	for i := 0; i < n; i++ {
		line, err := json.Marshal(newTestEvent("egress.deny"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("seed spool: %v", err)
	}
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	if sp.Lines() != n {
		t.Fatalf("seeded %d lines but the spool reports a backlog of %d", n, sp.Lines())
	}
	backlog := spoolSize(t, path)
	rec := &fakeRecorder{}
	passes, written := drainAll(t, sp, rec, batch)
	if rec.count() != n {
		t.Fatalf("N=%d: replayed %d events, want %d — the cheaper drain must still drain everything", n, rec.count(), n)
	}
	if lc := spoolLineCount(t, path); lc != 0 {
		t.Fatalf("N=%d: spool still holds %d lines after a full drain", n, lc)
	}
	t.Logf("N=%d: backlog=%d bytes, passes=%d, written=%d", n, backlog, passes, written)
	return float64(written) / float64(backlog)
}

// TestAuditSpoolLinesDoesNotWaitOnADrainPass pins the /metrics arm. Drain holds
// a.mu across every rec.Record, bounded only by spoolDrainDeadline, and Lines()
// used to take that same mutex. When the store is unreachable in the way the
// code itself names — an external session holding the audit chain advisory lock,
// so Record never answers — a scrape blocked for essentially the whole 15s pass,
// past Prometheus' 10s default scrape_timeout, and the ENTIRE /metrics response
// was lost: wardyn_store_up and the run counters with it, once per tick, during
// exactly the outage those gauges exist to report.
func TestAuditSpoolLinesDoesNotWaitOnADrainPass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := sp.Append(newTestEvent("run.kill")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	// The existing blockingRecorder: it stalls inside Record until its context
	// is done for one action — the store call that waits forever on the audit
	// chain advisory lock because an external session left a transaction open.
	rec := &blockingRecorder{block: "run.kill", entered: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sp.Drain(ctx, rec, 100)
	}()
	<-rec.entered // the drain now holds the spool lock inside Record

	start := time.Now()
	got := sp.Lines()
	waited := time.Since(start)
	cancel()
	<-done

	if waited > 100*time.Millisecond {
		t.Errorf("Lines() waited %v behind an in-flight drain pass; /metrics is served from this call, and a wait "+
			"beyond the scrape timeout loses the whole response — including the two gauges that distinguish a dead "+
			"store from an idle cluster", waited)
	}
	if got != 5 {
		t.Errorf("Lines() = %d during a drain, want the 5 still-unreplayed events", got)
	}
}

// TestAuditSpoolAppendAfterCompactionKeepsTheTornTailSeparator is the data-loss
// arm, and it is deliberately its own test rather than a clause in the
// throughput one: what it catches is a LOST EVENT, and no assertion about bytes
// written can see that.
//
// Drain replaces the spool inode by rename. It used to reopen the path
// AFTERWARDS with O_WRONLY, which had two consequences. The one an operator
// could hit every day: endsUnterminated probes the spool's last byte with
// ReadAt, which a write-only fd refuses, so from the first compaction onward the
// torn-tail separator silently stopped being applied — and a good event appended
// after a torn fragment was then read by the next Drain as ONE unparseable line
// and DROPPED, destroying an event whose primary-store write had already failed.
// That is C1 inverted: the invariant this file exists to hold, turned into
// silent loss. The other, on the error path, left a.f on the renamed-away inode
// so every later Append fsynced into a file with no directory entry and returned
// nil. Both are gone because the new fd is now opened on the temp file BEFORE
// the rename, with O_RDWR, and swapped in after it.
func TestAuditSpoolAppendAfterCompactionKeepsTheTornTailSeparator(t *testing.T) {
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
	if _, err := sp.Drain(context.Background(), rec, 2); err != nil {
		t.Fatalf("partial drain: %v", err)
	}
	if spoolSize(t, path) == 0 {
		t.Fatal("precondition: the partial drain left nothing to append to")
	}

	// An ENOSPC-style torn write: bytes land, the newline does not.
	tf, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open spool to tear it: %v", err)
	}
	if _, err := tf.Write([]byte(`{"id":"partial`)); err != nil {
		t.Fatalf("write torn fragment: %v", err)
	}
	tf.Close()

	survivor := newTestEvent("credential.mint")
	if err := sp.Append(survivor); err != nil {
		t.Fatalf("Append after the torn tail: %v", err)
	}

	// The fragment is unparseable and is dropped; the event written after it
	// must NOT be dropped with it.
	before := sp.TornDrops()
	if _, err := sp.Drain(context.Background(), rec, 100); err != nil {
		t.Fatalf("drain after tear: %v", err)
	}
	if sp.TornDrops() != before+1 {
		t.Errorf("torn drops went from %d to %d, want exactly one (the fragment)", before, sp.TornDrops())
	}
	for _, ev := range rec.got {
		if ev.ID == survivor.ID {
			return
		}
	}
	t.Fatalf("the event appended after a torn fragment was never replayed: the separator that keeps it on its own "+
		"line was not written, so Drain read `fragment{event}` as one unparseable line and destroyed an audit "+
		"event whose primary-store write had already failed (id=%s)", survivor.ID)
}

// TestAuditSpoolQuarantineCountSurvivesARestart pins the observability arm. The
// quarantine sidecar is PERSISTENT: it holds events the queryable trail is
// missing until an operator re-feeds them. The counter reporting that was
// per-process, so any restart — a deploy, a crash loop, a pod reschedule —
// returned wardyn_audit_spool_quarantined_total to 0 while the sidecar sat
// untouched, and /metrics then showed a completely healthy audit surface over a
// permanently incomplete trail. docs/OPERATIONS.md tells the operator to alert
// on that counter.
func TestAuditSpoolQuarantineCountSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}
	for _, ev := range []types.AuditEvent{newTestEvent("poison"), newTestEvent("good")} {
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
	if got := sp.Quarantined(); got != 1 {
		t.Fatalf("precondition: Quarantined() = %d before the restart, want 1", got)
	}

	// The restart: a new process opening the same spool path.
	restarted, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool after restart: %v", err)
	}
	if got := restarted.Quarantined(); got != 1 {
		t.Errorf("Quarantined() = %d after a restart, want 1 — the sidecar still holds the missing event, so the "+
			"only scrape-surface signal that the audit trail is permanently incomplete must outlive the process "+
			"that noticed", got)
	}
	// And the backlog gauge is rebuilt from disk too, so a restart mid-outage
	// does not report an empty spool over a file that is not.
	if got := restarted.Lines(); got != spoolLineCount(t, path) {
		t.Errorf("Lines() = %d after a restart, want the %d lines on disk", got, spoolLineCount(t, path))
	}
}
