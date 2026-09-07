// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestSpoolCursorRefusesAStaleCursorOverAReplacedSpool is F280.
//
// The cursor sidecar used to be trusted on a SIZE BOUND alone (`n < 0 || n >
// size`), while the comment above it claimed something a size bound cannot say:
// that "a cursor past the end of the file describes a file that no longer
// exists … and honouring it would SKIP un-replayed events, which is the one
// direction this file never errs in". An offset left over a REPLACED spool that
// happens to be at least as large is IN range, so it was honoured, and Drain
// began past lines nothing had replayed. Executed on the shipped code: 4 of 5
// events replayed, one credential.mint silently lost — a C1 violation, the
// invariant the whole file exists to hold.
//
// The sidecar is written the way the daemon itself would have written it for the
// ORIGINAL file — a well-formed, in-range, correctly-fingerprinted cursor — so
// what this pins is the IDENTITY check and not merely a change of file format.
func TestSpoolCursorRefusesAStaleCursorOverAReplacedSpool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := sp.Append(newTestEvent("run.create")); err != nil {
			t.Fatal(err)
		}
	}
	// The offset a real drain of the FIRST line would have left: a line
	// boundary, well inside the file, exactly what saveSpoolCursor persists.
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cursor := int64(bytes.IndexByte(buf, '\n') + 1)
	if cursor <= 0 {
		t.Fatal("fixture: the spool has no line terminator")
	}
	writeSpoolSidecar(t, path, cursor, spoolCursorFingerprint(path, cursor))

	// THE REPLACEMENT, and it is not a contrived one: an operator moving a
	// `<spool>.quarantine` file back onto the spool path is the documented
	// recovery for a quarantined line (docs/OPERATIONS.md's quarantine section
	// says the file "is a valid JSONL spool you can move back onto the spool
	// path once the cause is fixed"), and it produces exactly this — different
	// content at the same path, larger than the stale offset.
	const replacements = 5
	var fresh bytes.Buffer
	for range replacements {
		line, err := json.Marshal(newTestEvent("credential.mint"))
		if err != nil {
			t.Fatal(err)
		}
		fresh.Write(line)
		fresh.WriteByte('\n')
	}
	if int64(fresh.Len()) <= cursor {
		t.Fatalf("fixture: replacement is %d bytes, must exceed the stale cursor %d or the old size bound "+
			"would have caught it and the test would prove nothing", fresh.Len(), cursor)
	}
	if err := os.WriteFile(path, fresh.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeRecorder{}
	if _, err := reopened.Drain(context.Background(), rec, 100); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(); got != replacements {
		t.Errorf("replayed %d of %d events over a REPLACED spool: a cursor measured against different bytes "+
			"was honoured because it fell inside the new file's size, so Drain began past %d un-replayed audit "+
			"events. C1 permits a duplicate and never a silent loss", got, replacements, replacements-got)
	}
}

// TestSpoolCursorRefusesAStaleCursorOverACompactedSpool is F280's other arm: the
// crash window inside compact() itself.
//
// compact() renames a rewritten spool over the old one and only then retires the
// cursor. A crash in between leaves the OLD offset over the NEW file — and the
// new file is SMALLER, which is precisely the case a size bound waves through:
// the old cursor was measured against a file that contained the consumed prefix,
// the new one has that prefix removed, and the same number is now pointing into
// the middle of lines nobody has replayed.
//
// The fix is both halves — the fingerprint refuses it, and compact writes the 0
// BEFORE the rename so the window stops existing — and this pins the outcome
// they share: nothing is skipped.
func TestSpoolCursorRefusesAStaleCursorOverACompactedSpool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	const total, retired = 8, 2
	for range total {
		if err := sp.Append(newTestEvent("run.create")); err != nil {
			t.Fatal(err)
		}
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(bytes.TrimRight(buf, "\n"), []byte{'\n'})
	if len(lines) != total {
		t.Fatalf("fixture: %d lines, want %d", len(lines), total)
	}
	// A cursor that had retired the first two lines, fingerprinted against the
	// file as it stood — a cursor the daemon really would have written.
	var cursor int64
	for _, l := range lines[:retired] {
		cursor += int64(len(l))
	}
	writeSpoolSidecar(t, path, cursor, spoolCursorFingerprint(path, cursor))

	// …and the compaction that crashed after the rename: the retired lines are
	// gone, the file is smaller, and the stale cursor is still in range — which
	// is the case the old size bound waved through.
	compacted := append(bytes.Join(lines[retired:], nil), '\n')
	if err := os.WriteFile(path, compacted, 0o600); err != nil {
		t.Fatal(err)
	}
	if cursor > int64(len(compacted)) {
		t.Fatalf("fixture: cursor %d is past the compacted file (%d bytes), so the OLD size bound would have "+
			"refused it and this arm would prove nothing", cursor, len(compacted))
	}

	reopened, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeRecorder{}
	if _, err := reopened.Drain(context.Background(), rec, 100); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(); got != total-retired {
		t.Errorf("replayed %d of %d un-replayed events after a compaction that crashed before retiring the "+
			"cursor — the offset was in range for the compacted file, so it skipped lines that had never "+
			"reached the store", got, total-retired)
	}
}

// A cursor that still describes the file is still HONOURED. Without this the
// fix could be "always replay from 0", which trades a silent loss for the
// duplicate storm F219 measured (~32k duplicate rows on one restart of a 64k
// backlog) — and the pin above could not tell the two apart.
func TestSpoolCursorHonoursACursorThatStillDescribesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if err := sp.Append(newTestEvent("run.create")); err != nil {
			t.Fatal(err)
		}
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cursor := int64(bytes.IndexByte(buf, '\n') + 1)
	writeSpoolSidecar(t, path, cursor, spoolCursorFingerprint(path, cursor))

	if got := seedSpoolCursor(path+".consumed", path, int64(len(buf))); got != cursor {
		t.Errorf("seedSpoolCursor = %d over the very file it was written against, want %d — a cursor whose "+
			"content is still there must be honoured, or every restart replays the whole backlog", got, cursor)
	}

	reopened, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeRecorder{}
	if _, err := reopened.Drain(context.Background(), rec, 100); err != nil {
		t.Fatal(err)
	}
	if got := rec.count(); got != 3 {
		t.Errorf("replayed %d events, want the 3 the cursor had not retired — the first line already reached "+
			"the store", got)
	}
}

// The PRE-IDENTITY sidecar — a bare offset, which is what every daemon before
// this change wrote — asserts a position over a file nothing can tie it to. It
// is refused like any other unusable cursor, so the upgrade that first reads one
// costs a replay of the in-flight batch (at-least-once, the residual C1 accepts)
// and never the loss the unverified offset would license.
func TestSpoolCursorRefusesThePreIdentitySidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := sp.Append(newTestEvent("run.create")); err != nil {
			t.Fatal(err)
		}
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cursor := int64(bytes.IndexByte(buf, '\n') + 1)
	if err := os.WriteFile(path+".consumed", fmt.Appendf(nil, "%d", cursor), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := seedSpoolCursor(path+".consumed", path, int64(len(buf))); got != 0 {
		t.Errorf("a one-field (pre-identity) sidecar seeded %d, want 0 — an offset with nothing tying it to "+
			"this file's content is exactly the trust F280 removed", got)
	}

	// A fingerprint that does not match the file is refused too, which is the
	// same rule stated over the format that DOES carry one.
	writeSpoolSidecar(t, path, cursor, "deadbeef")
	if got := seedSpoolCursor(path+".consumed", path, int64(len(buf))); got != 0 {
		t.Errorf("a sidecar whose fingerprint does not match the spool seeded %d, want 0", got)
	}

	// …and the anchor really is read at the OFFSET, not at byte 0: a file that
	// shares its head with the original but diverges before the cursor must not
	// be accepted. (Both files here are the same length, so the size bound is
	// blind to the difference by construction.)
	writeSpoolSidecar(t, path, cursor, spoolCursorFingerprint(path, cursor))
	head := buf[:cursor]
	tail := bytes.Repeat([]byte{'x'}, len(buf)-len(head)-1)
	if err := os.WriteFile(path, append(append([]byte{}, head...), append(tail, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := seedSpoolCursor(path+".consumed", path, int64(len(buf))); got != cursor {
		t.Errorf("seedSpoolCursor = %d, want %d — the bytes BEFORE the cursor are unchanged, so the cursor "+
			"still describes what it claims to have replayed and must survive an edit behind it", got, cursor)
	}
}

// A compaction retires the cursor rather than leaving the old offset standing.
func TestSpoolCompactionRetiresTheCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	sp.mu.Lock()
	err = sp.compact([][]byte{[]byte(`{"kept":true}`)}, 0, 0)
	sp.mu.Unlock()
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	got, err := os.ReadFile(path + ".consumed")
	if err != nil {
		t.Fatalf("no cursor sidecar after a compaction: %v", err)
	}
	if want := "0 " + spoolCursorNoAnchor; string(got) != want {
		t.Errorf("cursor sidecar = %q after a compaction, want %q — the file the old offset described is gone, "+
			"and the new one starts un-replayed at byte 0", got, want)
	}
	if sp.consumed != 0 {
		t.Errorf("in-memory cursor = %d after a compaction, want 0", sp.consumed)
	}
}

func writeSpoolSidecar(t *testing.T, spoolPath string, offset int64, fingerprint string) {
	t.Helper()
	if err := os.WriteFile(spoolPath+".consumed", fmt.Appendf(nil, "%d %s", offset, fingerprint), 0o600); err != nil {
		t.Fatal(err)
	}
}
