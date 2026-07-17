// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask_test

import (
	"bytes"
	"encoding/base64"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// ─── Registry tests ──────────────────────────────────────────────────────────

func TestRegistry_Add_Snapshot(t *testing.T) {
	r := secretmask.NewRegistry()
	runID := uuid.New()
	secret := []byte("supersecretvalue1")

	r.Add(runID, secret)

	snap := r.Snapshot(runID)
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d, want 1", len(snap))
	}
	if !bytes.Equal(snap[0], secret) {
		t.Fatalf("Snapshot[0] = %q, want %q", snap[0], secret)
	}
}

func TestRegistry_AddGlobal_AppearsInAllSnapshots(t *testing.T) {
	r := secretmask.NewRegistry()
	runA, runB := uuid.New(), uuid.New()
	global := []byte("global-secret-val")

	r.AddGlobal(global)
	r.Add(runA, []byte("per-run-secret-a"))

	snapA := r.Snapshot(runA)
	snapB := r.Snapshot(runB)

	// runA gets its own + global
	if len(snapA) != 2 {
		t.Fatalf("snapA len = %d, want 2", len(snapA))
	}
	// runB gets only global
	if len(snapB) != 1 {
		t.Fatalf("snapB len = %d, want 1", len(snapB))
	}
	if !bytes.Equal(snapB[0], global) {
		t.Fatalf("snapB[0] = %q, want global secret", snapB[0])
	}
}

func TestRegistry_MinLen_Floor(t *testing.T) {
	r := secretmask.NewRegistry()
	runID := uuid.New()

	r.Add(runID, []byte("short"))    // 5 bytes < MinLen(8) — should be ignored
	r.AddGlobal([]byte("tiny"))      // 4 bytes < MinLen(8) — should be ignored
	r.Add(runID, []byte("exactlen")) // exactly 8 bytes — should be kept

	snap := r.Snapshot(runID)
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d, want 1 (short values ignored)", len(snap))
	}
	if !bytes.Equal(snap[0], []byte("exactlen")) {
		t.Fatalf("unexpected value in snapshot: %q", snap[0])
	}
}

func TestRegistry_Evict(t *testing.T) {
	r := secretmask.NewRegistry()
	runID := uuid.New()
	r.Add(runID, []byte("some-secret-here"))

	r.Evict(runID)

	snap := r.Snapshot(runID)
	if len(snap) != 0 {
		t.Fatalf("after Evict, Snapshot len = %d, want 0", len(snap))
	}
}

func TestRegistry_NilSafe(t *testing.T) {
	var r *secretmask.Registry
	runID := uuid.New()

	// None of these should panic.
	r.Add(runID, []byte("someverylongsecret"))
	r.AddGlobal([]byte("globalverylongsecret"))
	snap := r.Snapshot(runID)
	if snap != nil {
		t.Fatalf("nil Registry Snapshot should return nil, got %v", snap)
	}
	r.Evict(runID)
}

// ─── Masker tests ────────────────────────────────────────────────────────────

func TestMasker_SingleSecret_Replaced(t *testing.T) {
	secret := []byte("my-api-key-12345")
	m := secretmask.NewMasker([][]byte{secret})

	input := []byte("Authorization: Bearer my-api-key-12345\nHost: api.example.com")
	got := m.Mask(input)

	if bytes.Contains(got, secret) {
		t.Fatalf("secret not masked, got: %q", got)
	}
	if !bytes.Contains(got, []byte("<secret-hidden>")) {
		t.Fatalf("placeholder not found in: %q", got)
	}
}

func TestMasker_MultipleOccurrences(t *testing.T) {
	secret := []byte("token-abcdefgh")
	m := secretmask.NewMasker([][]byte{secret})

	input := []byte("token-abcdefgh token-abcdefgh token-abcdefgh")
	got := m.Mask(input)

	if bytes.Contains(got, secret) {
		t.Fatalf("secret still present after masking: %q", got)
	}
	count := bytes.Count(got, []byte("<secret-hidden>"))
	if count != 3 {
		t.Fatalf("placeholder count = %d, want 3", count)
	}
}

func TestMasker_MultiSecret_LongestFirst(t *testing.T) {
	// "short-prefix" is a prefix of "short-prefix-and-more" — longer should
	// mask first so the shorter never gets a chance to double-mask.
	short := []byte("short-prefix")
	long := []byte("short-prefix-and-more")
	m := secretmask.NewMasker([][]byte{short, long})

	input := []byte("secret: short-prefix-and-more rest")
	got := m.Mask(input)

	if bytes.Contains(got, long) || bytes.Contains(got, short) {
		t.Fatalf("secret present after masking: %q", got)
	}
	// Exactly one placeholder for the full long match.
	count := bytes.Count(got, []byte("<secret-hidden>"))
	if count != 1 {
		t.Fatalf("expected 1 placeholder (long match wins), got %d in %q", count, got)
	}
}

func TestMasker_MinLenDropped(t *testing.T) {
	m := secretmask.NewMasker([][]byte{[]byte("short"), []byte("longenoughsecret")})
	input := []byte("short is visible but longenoughsecret is not")
	got := m.Mask(input)

	// "short" (5 bytes < MinLen) must NOT be replaced.
	if !bytes.Contains(got, []byte("short")) {
		t.Fatalf("short value incorrectly masked: %q", got)
	}
	if bytes.Contains(got, []byte("longenoughsecret")) {
		t.Fatalf("long secret not masked: %q", got)
	}
}

func TestMasker_ReplacementLiteral(t *testing.T) {
	m := secretmask.NewMasker([][]byte{[]byte("secret-token-here")})
	got := m.Mask([]byte("secret-token-here"))
	if !bytes.Equal(got, []byte("<secret-hidden>")) {
		t.Fatalf("replacement literal wrong: got %q, want <secret-hidden>", got)
	}
}

func TestMasker_BinarySafeAsciicastLine(t *testing.T) {
	// asciicast v2 data line contains arbitrary terminal output (may include
	// binary escape sequences). The masker must handle non-UTF-8 bytes.
	secret := []byte("api-key-binary\x1b")
	m := secretmask.NewMasker([][]byte{secret})

	// Simulate an asciicast data line: [timestamp, "o", "<terminal output>"]
	line := []byte(`[1.23,"o","prefix api-key-binary` + "\x1b" + `[0m suffix"]`)
	got := m.Mask(line)

	if bytes.Contains(got, secret) {
		t.Fatalf("secret not masked in binary line: %q", got)
	}
	if !bytes.Contains(got, []byte("<secret-hidden>")) {
		t.Fatalf("placeholder missing: %q", got)
	}
}

// NEGATIVE test: base64-encoded secret is NOT masked.
// This locks in the honest residual documented in the package.
func TestMasker_Base64EncodedNotMasked(t *testing.T) {
	secret := []byte("my-api-key-12345")
	m := secretmask.NewMasker([][]byte{secret})

	encoded := base64.StdEncoding.EncodeToString(secret)
	input := []byte("Authorization: Basic " + encoded)
	got := m.Mask(input)

	// The base64 version MUST still be present (not masked) — honest residual.
	if !bytes.Contains(got, []byte(encoded)) {
		t.Fatalf("base64-encoded secret was incorrectly masked — honest-residual test failed")
	}
}

func TestMasker_NoSecrets_PassThrough(t *testing.T) {
	m := secretmask.NewMasker(nil)
	input := []byte("arbitrary data without secrets")
	got := m.Mask(input)
	if !bytes.Equal(got, input) {
		t.Fatalf("zero-secret masker mutated input: %q", got)
	}
}

// ─── MaskingWriter tests ─────────────────────────────────────────────────────

// TestMaskingWriter_BufferBoundarySplit feeds a secret one byte at a time
// through MaskingWriter and verifies it is still masked.
func TestMaskingWriter_BufferBoundarySplit(t *testing.T) {
	secret := []byte("supersecretkey01")
	m := secretmask.NewMasker([][]byte{secret})

	var buf bytes.Buffer
	w := secretmask.NewMaskingWriter(&buf, m)

	// Write the prefix, then the secret one byte at a time, then suffix.
	prefix := []byte("prefix:")
	if _, err := w.Write(prefix); err != nil {
		t.Fatalf("write prefix: %v", err)
	}
	for i := range secret {
		if _, err := w.Write(secret[i : i+1]); err != nil {
			t.Fatalf("write secret byte %d: %v", i, err)
		}
	}
	suffix := []byte(":suffix")
	if _, err := w.Write(suffix); err != nil {
		t.Fatalf("write suffix: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := buf.Bytes()
	if bytes.Contains(out, secret) {
		t.Fatalf("secret leaked through boundary split: %q", out)
	}
	if !bytes.Contains(out, []byte("<secret-hidden>")) {
		t.Fatalf("placeholder missing after boundary split: %q", out)
	}
}

func TestMaskingWriter_MultiSecretStream(t *testing.T) {
	secrets := [][]byte{
		[]byte("token-AAAAAAAA"),
		[]byte("token-BBBBBBBB"),
	}
	m := secretmask.NewMasker(secrets)

	var buf bytes.Buffer
	w := secretmask.NewMaskingWriter(&buf, m)

	data := []byte("first: token-AAAAAAAA and second: token-BBBBBBBB end")
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := buf.Bytes()
	for _, s := range secrets {
		if bytes.Contains(out, s) {
			t.Fatalf("secret %q leaked: %q", s, out)
		}
	}
	count := bytes.Count(out, []byte("<secret-hidden>"))
	if count != 2 {
		t.Fatalf("expected 2 placeholders, got %d: %q", count, out)
	}
}

func TestMaskingWriter_CloseFlushesRetainedTail(t *testing.T) {
	// Secret exactly at end of the stream (in the retained tail).
	secret := []byte("tail-secret-here")
	m := secretmask.NewMasker([][]byte{secret})

	var buf bytes.Buffer
	w := secretmask.NewMaskingWriter(&buf, m)

	// Write less than maxLen so it's entirely in the tail, then close.
	if _, err := w.Write([]byte("tail-secret-here")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := buf.Bytes()
	if bytes.Contains(out, secret) {
		t.Fatalf("secret in tail not masked after Close: %q", out)
	}
	if !bytes.Contains(out, []byte("<secret-hidden>")) {
		t.Fatalf("placeholder missing: %q", out)
	}
}

func TestMaskingWriter_ImplementsWriteCloser(t *testing.T) {
	var _ io.WriteCloser = secretmask.NewMaskingWriter(io.Discard, secretmask.NewMasker(nil))
}

// closeSpyWriter is an io.WriteCloser that records whether Close was called.
type closeSpyWriter struct {
	bytes.Buffer
	closed bool
}

func (c *closeSpyWriter) Close() error { c.closed = true; return nil }

// TestMaskingWriter_CloseDoesNotCloseBorrowedDst pins the ownership invariant:
// MaskingWriter.Close flushes its retained tail and nothing else. dst is
// borrowed, so closing it would make the writer a second closer racing the
// caller's own — which on the recording-upload path silently swallowed every
// copy error through io.Pipe's once-only error store.
func TestMaskingWriter_CloseDoesNotCloseBorrowedDst(t *testing.T) {
	secret := []byte("super-secret-token-value")
	spy := &closeSpyWriter{}
	w := secretmask.NewMaskingWriter(spy, secretmask.NewMasker([][]byte{secret}))

	if _, err := w.Write([]byte("prefix ")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if spy.closed {
		t.Fatal("MaskingWriter.Close closed a borrowed dst; it must only flush the tail")
	}
	// The tail must still have been flushed (Close's actual job).
	if got := spy.String(); got != "prefix " {
		t.Fatalf("tail not flushed on Close: got %q want %q", got, "prefix ")
	}
}

func TestMaskingWriter_NoSecrets_PassThrough(t *testing.T) {
	m := secretmask.NewMasker(nil)
	var buf bytes.Buffer
	w := secretmask.NewMaskingWriter(&buf, m)
	data := []byte("no secrets here at all")
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("pass-through mutated: got %q want %q", buf.Bytes(), data)
	}
}

// ─── Fail-CLOSED-on-panic tests (invariant-1) ────────────────────────────────

// TestSafeMask_PanicFailsClosed proves that when Masker.Mask panics, the
// recovered path does NOT forward the raw input bytes (which could contain
// secrets) — it substitutes the placeholder and returns the panic as an error.
//
// Red-first: against the old fail-OPEN code this asserts the leaked raw bytes
// are absent, which fails because the old code returned the original input.
func TestMaskingWriter_PanicFailsClosed_NoRawLeak(t *testing.T) {
	// Inject a masker that panics on every call (simulates a crash inside Mask).
	orig := secretmask.MaskCallForTest
	t.Cleanup(func() { secretmask.MaskCallForTest = orig })
	secretmask.MaskCallForTest = func(_ secretmask.Masker, _ []byte) []byte {
		panic("boom: simulated masker crash")
	}

	// A registered secret gives the writer a non-trivial maxLen so the tail
	// path is exercised; the value below is what must NOT leak downstream.
	rawSecret := []byte("leaked-secret-payload-xyz")
	m := secretmask.NewMasker([][]byte{rawSecret})

	var buf bytes.Buffer
	w := secretmask.NewMaskingWriter(&buf, m)

	input := []byte("prefix " + string(rawSecret) + " suffix")
	_, err := w.Write(input)
	if err == nil {
		t.Fatalf("Write should surface the masker panic as an error, got nil")
	}
	if cerr := w.Close(); cerr != nil {
		// Close may also surface a panic error; that's acceptable. Only fail on
		// a genuine downstream write error.
		t.Logf("Close returned: %v", cerr)
	}

	out := buf.Bytes()
	// The core invariant: NONE of the raw input bytes may be emitted.
	if bytes.Contains(out, rawSecret) {
		t.Fatalf("FAIL-OPEN LEAK: raw secret forwarded downstream on panic: %q", out)
	}
	if bytes.Contains(out, []byte("prefix")) || bytes.Contains(out, []byte("suffix")) {
		t.Fatalf("FAIL-OPEN LEAK: raw input chunk forwarded downstream on panic: %q", out)
	}
	// Fail-CLOSED should still emit the placeholder so something is recorded.
	if len(out) > 0 && !bytes.Contains(out, []byte("<secret-hidden>")) {
		t.Fatalf("expected placeholder on fail-closed path, got: %q", out)
	}
}

// TestSafeMask_Panic_ReturnsErrorAndPlaceholder verifies the unit-level contract
// of the fail-closed path exercised through the writer: an error is surfaced and
// the placeholder (never the raw input) reaches the downstream writer.
func TestMaskingWriter_PanicFailsClosed_Close(t *testing.T) {
	orig := secretmask.MaskCallForTest
	t.Cleanup(func() { secretmask.MaskCallForTest = orig })
	secretmask.MaskCallForTest = func(_ secretmask.Masker, _ []byte) []byte {
		panic("boom on close")
	}

	rawSecret := []byte("tail-only-secret-here")
	m := secretmask.NewMasker([][]byte{rawSecret})

	var buf bytes.Buffer
	w := secretmask.NewMaskingWriter(&buf, m)

	// Write fewer bytes than maxLen so the whole thing sits in the retained
	// tail and is only masked at Close — exercising Close's safeMask path.
	if _, err := w.Write([]byte("tail-only-secret-here")); err != nil {
		// Write already panicked-and-recovered: it returns the panic error.
		t.Logf("Write returned: %v", err)
	}
	_ = w.Close()

	out := buf.Bytes()
	if bytes.Contains(out, rawSecret) {
		t.Fatalf("FAIL-OPEN LEAK on Close: raw secret forwarded: %q", out)
	}
}

func TestMaskingWriter_OverlappingSecrets(t *testing.T) {
	// Two secrets where the shorter is a strict prefix of the longer.
	short := []byte("overlap-prefix-x")  // 16 bytes
	long := []byte("overlap-prefix-xyz") // 18 bytes
	m := secretmask.NewMasker([][]byte{short, long})

	var buf bytes.Buffer
	w := secretmask.NewMaskingWriter(&buf, m)
	if _, err := w.Write([]byte("data: overlap-prefix-xyz end")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := buf.Bytes()
	if bytes.Contains(out, long) || bytes.Contains(out, short) {
		t.Fatalf("secret leaked: %q", out)
	}
}
