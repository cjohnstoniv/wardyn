// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// TestCastWriter_HeaderAndEvents asserts the asciicast v2 shape: a header object
// on line 1 ({"version":2,...}) then one ["t","o","data"] event per Write.
func TestCastWriter_HeaderAndEvents(t *testing.T) {
	var buf bytes.Buffer
	start := time.Unix(1700000000, 0)
	cw := NewCastWriter(&buf, 120, 40, start)

	if _, err := cw.Write([]byte("hello ")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := cw.Write([]byte("world\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (header + 2 events), got %d:\n%s", len(lines), buf.String())
	}

	// Line 1: header.
	var hdr CastHeader
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatalf("header not valid JSON: %v (%q)", err, lines[0])
	}
	if hdr.Version != 2 || hdr.Width != 120 || hdr.Height != 40 {
		t.Errorf("header = %+v, want version=2 width=120 height=40", hdr)
	}
	if hdr.Timestamp != start.Unix() {
		t.Errorf("header timestamp = %d, want %d", hdr.Timestamp, start.Unix())
	}

	// Lines 2+: events ["t","o","data"] with a NUMERIC time and "o" code.
	for _, ln := range lines[1:] {
		var ev []json.RawMessage
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("event not a JSON array: %v (%q)", err, ln)
		}
		if len(ev) != 3 {
			t.Fatalf("event must have 3 elements, got %d (%q)", len(ev), ln)
		}
		var ts float64
		if err := json.Unmarshal(ev[0], &ts); err != nil {
			t.Errorf("event time must be a number: %v (%q)", err, ev[0])
		}
		var code string
		_ = json.Unmarshal(ev[1], &code)
		if code != "o" {
			t.Errorf("event code = %q, want \"o\"", code)
		}
	}

	// Reassemble the output payloads and confirm round-trip fidelity.
	var got strings.Builder
	for _, ln := range lines[1:] {
		var ev []json.RawMessage
		_ = json.Unmarshal([]byte(ln), &ev)
		var data string
		_ = json.Unmarshal(ev[2], &data)
		got.WriteString(data)
	}
	if got.String() != "hello world\r\n" {
		t.Errorf("reassembled output = %q, want %q", got.String(), "hello world\r\n")
	}
}

// TestCastWriter_SplitRuneAcrossWrites is the regression test for a
// multi-byte UTF-8 rune whose bytes straddle two adjacent PTY reads. Naively
// converting each write's bytes to a string independently mangles the rune:
// json.Marshal silently replaces each invalid fragment with U+FFFD, so
// without buffering the reassembled output would read "hello ��!"
// instead of "hello 世!".
func TestCastWriter_SplitRuneAcrossWrites(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCastWriter(&buf, 80, 24, time.Now())

	full := "hello 世!"
	b := []byte(full)
	idx := strings.IndexRune(full, '世')
	split := idx + 2 // first two of the rune's three UTF-8 bytes

	if _, err := cw.Write(b[:split]); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if _, err := cw.Write(b[split:]); err != nil {
		t.Fatalf("Write 2: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	var got strings.Builder
	for _, ln := range lines[1:] { // skip the header line
		var ev []json.RawMessage
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("event not a JSON array: %v (%q)", err, ln)
		}
		var data string
		if err := json.Unmarshal(ev[2], &data); err != nil {
			t.Fatalf("event payload: %v (%q)", err, ln)
		}
		got.WriteString(data)
	}
	if got.String() != full {
		t.Errorf("reassembled output = %q, want %q", got.String(), full)
	}
}

func TestCastWriter_HadOutput(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCastWriter(&buf, 80, 24, time.Now())
	if cw.HadOutput() {
		t.Error("HadOutput should be false before any Write")
	}
	_, _ = cw.Write([]byte("x"))
	if !cw.HadOutput() {
		t.Error("HadOutput should be true after a Write")
	}
}

func TestCastWriter_DefaultGeometry(t *testing.T) {
	var buf bytes.Buffer
	NewCastWriter(&buf, 0, 0, time.Unix(0, 0))
	var hdr CastHeader
	line := strings.SplitN(buf.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(line), &hdr); err != nil {
		t.Fatalf("header: %v", err)
	}
	if hdr.Width != 80 || hdr.Height != 24 {
		t.Errorf("default geometry = %dx%d, want 80x24", hdr.Width, hdr.Height)
	}
}

// TestSaveCastNamed_PerSessionKeying asserts an interactive session cast is keyed
// per run+session so it does NOT clobber the batch run's cast (bare runID) and
// two sessions of the same run coexist.
func TestSaveCastNamed_PerSessionKeying(t *testing.T) {
	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	const runID = "550e8400-e29b-41d4-a716-446655440000"

	// Batch run cast (bare runID).
	if err := store.SaveCast(ctx, runID, strings.NewReader("batch")); err != nil {
		t.Fatalf("SaveCast: %v", err)
	}
	// Two interactive session casts.
	if err := store.SaveCastNamed(ctx, runID, "sessA", strings.NewReader("A")); err != nil {
		t.Fatalf("SaveCastNamed A: %v", err)
	}
	if err := store.SaveCastNamed(ctx, runID, "sessB", strings.NewReader("B")); err != nil {
		t.Fatalf("SaveCastNamed B: %v", err)
	}

	// The batch cast must be untouched by the session writes.
	if got := readKey(t, store, runID); got != "batch" {
		t.Errorf("batch cast clobbered: got %q, want %q", got, "batch")
	}
	if got := readKey(t, store, CastKey(runID, "sessA")); got != "A" {
		t.Errorf("sessA cast = %q, want A", got)
	}
	if got := readKey(t, store, CastKey(runID, "sessB")); got != "B" {
		t.Errorf("sessB cast = %q, want B", got)
	}
}

// TestSaveCastNamed_RejectsTraversalSuffix: a session suffix that smuggles a
// path separator / traversal must be rejected (fail closed).
func TestSaveCastNamed_RejectsTraversalSuffix(t *testing.T) {
	store, _ := NewFSStore(t.TempDir())
	ctx := context.Background()
	bad := []string{"../escape", "a/b", "a\\b", "..", "x~y", "a\x00b"}
	for _, s := range bad {
		if err := store.SaveCastNamed(ctx, "run", s, strings.NewReader("x")); err == nil {
			t.Errorf("SaveCastNamed suffix %q should be rejected", s)
		}
	}
}

func readKey(t *testing.T, store Store, key string) string {
	t.Helper()
	rc, err := store.OpenCast(context.Background(), key)
	if err != nil {
		t.Fatalf("OpenCast(%q): %v", key, err)
	}
	defer rc.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(rc)
	return b.String()
}

// TestReadCapped covers the pg store's bounded read (pgstore.go): the growth
// strategy there exists to keep a hostile max-size cast from OOMKilling the
// control plane, so the contract it has to keep is exact — everything under
// the cap round-trips byte-for-byte, and anything over it comes back as
// limit+1 bytes so SaveCast rejects it BEFORE the INSERT (no partial row).
func TestReadCapped(t *testing.T) {
	const limit = 8
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"under", "abc", 3},
		{"exactly at the cap", "12345678", 8},
		{"over the cap", "123456789abcdef", limit + 1},
		{"empty", "", 0},
	} {
		got, err := readCapped(iotest.OneByteReader(strings.NewReader(tc.in)), limit)
		if err != nil {
			t.Fatalf("%s: readCapped: %v", tc.name, err)
		}
		if len(got) != tc.want || !strings.HasPrefix(tc.in, string(got)) {
			t.Errorf("%s: readCapped = %q (%d bytes), want the first %d bytes of %q", tc.name, got, len(got), tc.want, tc.in)
		}
	}

	if _, err := readCapped(iotest.TimeoutReader(strings.NewReader("abcd")), limit); err == nil {
		t.Error("readCapped must propagate a mid-stream read error, not truncate silently")
	}
}

// TestReadCapped_GrowsInOneStep pins the strategy, not just the contract. Every
// case above stays inside the initial 64 KiB buffer, so none of them reach the
// regrow — and a growth chain that reallocated repeatedly would satisfy them all
// while reintroducing the 2.5x memory peak readCapped exists to avoid. A payload
// that outgrows the start must land in a buffer sized exactly limit+1: one step,
// two allocations total, whatever the input size.
func TestReadCapped_GrowsInOneStep(t *testing.T) {
	const limit = 70 << 10 // above the 64 KiB start, so the regrow must happen
	in := strings.Repeat("a", 65<<10)

	got, err := readCapped(strings.NewReader(in), limit)
	if err != nil {
		t.Fatalf("readCapped: %v", err)
	}
	if string(got) != in {
		t.Fatalf("readCapped returned %d bytes, want the %d-byte input back", len(got), len(in))
	}
	if cap(got) != limit+1 {
		t.Errorf("buffer cap = %d, want %d: the grow step must go straight to limit+1, not chain reallocations", cap(got), limit+1)
	}
}
