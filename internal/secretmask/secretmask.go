// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretmask implements Wardyn's output-masking layer for PTY capture
// and asciicast streams. It replaces verbatim byte-identical occurrences of
// registered secret values with the literal placeholder "<secret-hidden>".
//
// HONEST RESIDUAL: masking catches verbatim (byte-identical) leakage only.
// base64-encoded, hex-encoded, model-narrated, or otherwise transformed
// representations of the secret are NOT caught. This is intentional and
// documented here so the limitation is visible at the implementation site.
//
// Fail-CLOSED policy on masker panic: if Masker.Mask panics, the recovered
// panic is surfaced as an error AND the affected chunk is replaced with the
// placeholder instead of being forwarded verbatim. We must never emit raw,
// unmasked input bytes on a masker crash — that would leak the very secrets
// this layer exists to hide (invariant-1: the secrets path fails closed).
package secretmask

import (
	"bytes"
	"io"
	"sort"
	"sync"

	"github.com/google/uuid"
)

// MinLen is the minimum byte length a secret must have to be registered.
// Values shorter than this are silently ignored. Short strings risk false-
// positive masking (e.g. "ok", "id", common abbreviations).
const MinLen = 8

// placeholder is the literal bytes written in place of a masked secret.
var placeholder = []byte("<secret-hidden>")

// Registry is a thread-safe store of secret byte slices keyed by run UUID
// plus a process-global set that is applied on every masking call.
//
// A nil *Registry is safe: all methods on a nil pointer are no-ops.
type Registry struct {
	mu      sync.RWMutex
	perRun  map[uuid.UUID][][]byte // run id -> set of secret values
	globals [][]byte               // process-wide secrets applied to every run
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{perRun: make(map[uuid.UUID][][]byte)}
}

// Add registers value as a secret for runID. Values shorter than MinLen are
// ignored. The value is copied so the caller may reuse the backing array.
// Never logs the secret value.
func (r *Registry) Add(runID uuid.UUID, value []byte) {
	if r == nil || len(value) < MinLen {
		return
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	r.mu.Lock()
	r.perRun[runID] = append(r.perRun[runID], cp)
	r.mu.Unlock()
}

// AddGlobal registers value as a process-global secret applied on every run.
// Values shorter than MinLen are ignored.
func (r *Registry) AddGlobal(value []byte) {
	if r == nil || len(value) < MinLen {
		return
	}
	cp := make([]byte, len(value))
	copy(cp, value)
	r.mu.Lock()
	r.globals = append(r.globals, cp)
	r.mu.Unlock()
}

// Snapshot returns the combined set of secrets for runID (per-run + global).
// The returned slice is a copy; callers may use it without holding a lock.
func (r *Registry) Snapshot(runID uuid.UUID) [][]byte {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	perRun := r.perRun[runID]
	globals := r.globals
	r.mu.RUnlock()

	out := make([][]byte, 0, len(perRun)+len(globals))
	for _, v := range perRun {
		cp := make([]byte, len(v))
		copy(cp, v)
		out = append(out, cp)
	}
	for _, v := range globals {
		cp := make([]byte, len(v))
		copy(cp, v)
		out = append(out, cp)
	}
	return out
}

// Evict removes all per-run secrets for runID. Process-global secrets are
// unaffected. Idempotent.
func (r *Registry) Evict(runID uuid.UUID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.perRun, runID)
	r.mu.Unlock()
}

// ─── Masker ──────────────────────────────────────────────────────────────────

// Masker applies multi-secret exact masking over a fixed set of values.
// Build it from a Snapshot and reuse it across calls (immutable after
// construction). It is safe for concurrent use.
type Masker struct {
	// secrets sorted longest-first so overlapping values mask cleanly.
	secrets [][]byte
	// maxLen is the length of the longest secret (0 if no secrets).
	maxLen int
}

// NewMasker builds a Masker from the given secret values. Values shorter than
// MinLen are silently dropped. The returned Masker is immutable.
func NewMasker(secrets [][]byte) Masker {
	var kept [][]byte
	for _, s := range secrets {
		if len(s) >= MinLen {
			cp := make([]byte, len(s))
			copy(cp, s)
			kept = append(kept, cp)
		}
	}
	// Sort longest first for deterministic overlap handling.
	sort.Slice(kept, func(i, j int) bool { return len(kept[i]) > len(kept[j]) })

	maxLen := 0
	for _, s := range kept {
		if len(s) > maxLen {
			maxLen = len(s)
		}
	}
	return Masker{secrets: kept, maxLen: maxLen}
}

// Mask replaces all verbatim occurrences of each registered secret in p with
// "<secret-hidden>". Exact byte match only — no regex, no entropy scoring.
// The longest registered secret is tried first to handle overlapping values.
// Returns p unchanged if no secrets are registered.
//
// HONEST RESIDUAL: base64/hex/encoded/model-narrated representations are NOT
// masked. This only catches verbatim byte-identical leakage.
func (m Masker) Mask(p []byte) []byte {
	if len(m.secrets) == 0 {
		return p
	}
	out := p
	for _, s := range m.secrets {
		if bytes.Contains(out, s) {
			out = bytes.ReplaceAll(out, s, placeholder)
		}
	}
	return out
}

// ─── MaskingWriter ───────────────────────────────────────────────────────────

// MaskingWriter wraps a downstream io.Writer and masks secret values in the
// stream, even when a secret spans two adjacent Write calls (e.g. chunked PTY
// or asciicast frames).
//
// The correctness invariant: after each Write call, we retain a tail of
// (maxSecretLen-1) bytes from the (already-masked) buffer. On the next Write
// the retained tail is prepended to the new chunk before masking, then the
// fully-masked result minus the new tail is forwarded downstream. Close()
// flushes the retained tail.
//
// A nil downstream or a Masker with no secrets is safe (pass-through).
type MaskingWriter struct {
	m    Masker
	dst  io.Writer
	tail []byte // pending bytes not yet forwarded
}

// NewMaskingWriter wraps dst with masking. If m has no secrets the writer
// still works correctly (pass-through without masking overhead).
func NewMaskingWriter(dst io.Writer, m Masker) *MaskingWriter {
	return &MaskingWriter{m: m, dst: dst}
}

// Write masks p (with any retained tail prepended) and forwards the safe
// prefix downstream, retaining a tail of up to (maxSecretLen-1) bytes.
//
// Fail-CLOSED on masker panic: a recovered panic causes the placeholder (not
// the raw input) to be written downstream, then returns the panic as an error
// so the caller can record the anomaly. No unmasked secret bytes are emitted.
func (w *MaskingWriter) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Append new data to retained tail to detect cross-boundary secrets.
	buf := append(w.tail, p...) //nolint:gocritic // intentional re-use

	// Apply masking with panic recovery (fail-closed: see safeMask).
	masked, panicErr := safeMask(w.m, buf)

	if w.m.maxLen <= 1 {
		// No tail needed when there is at most one-byte overlap.
		w.tail = nil
		if _, werr := w.dst.Write(masked); werr != nil {
			return 0, werr
		}
		return len(p), panicErr
	}

	// Retain up to (maxLen-1) bytes so the next write can detect splits.
	tailLen := w.m.maxLen - 1
	if tailLen > len(masked) {
		tailLen = len(masked)
	}
	forward := masked[:len(masked)-tailLen]
	w.tail = make([]byte, tailLen)
	copy(w.tail, masked[len(masked)-tailLen:])

	if len(forward) > 0 {
		if _, werr := w.dst.Write(forward); werr != nil {
			return 0, werr
		}
	}
	return len(p), panicErr
}

// Close flushes the retained tail (after a final mask pass) to the downstream
// writer. If the downstream writer implements io.Closer it is also closed.
func (w *MaskingWriter) Close() error {
	if len(w.tail) > 0 {
		masked, _ := safeMask(w.m, w.tail)
		if _, err := w.dst.Write(masked); err != nil {
			return err
		}
		w.tail = nil
	}
	if c, ok := w.dst.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// MaskCallForTest is a package-level seam so that tests can inject a panicking
// masker and prove the fail-CLOSED path never emits raw input. Production code
// leaves it at the default, which simply calls Masker.Mask. It is exported only
// because the test lives in the external secretmask_test package; it must NOT be
// reassigned outside of tests.
var MaskCallForTest = func(m Masker, p []byte) []byte { return m.Mask(p) }

// safeMask wraps Mask with panic recovery. On a recovered panic it FAILS CLOSED:
// it returns the placeholder (NOT the original bytes) so no unmasked secret can
// leak downstream, plus an error so the caller can record the anomaly.
//
// Fail-CLOSED on panic (invariant-1): we deliberately drop the entire chunk's
// real content and substitute a single placeholder. Forwarding p verbatim here
// would defeat the whole masking layer — a crash in Mask must never become a
// secret-disclosure channel.
func safeMask(m Masker, p []byte) (out []byte, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			// Fail-CLOSED: substitute the placeholder for the affected chunk so
			// the raw (possibly secret-bearing) input bytes are never written.
			out = placeholder
			// Surface the panic as a non-fatal error for the caller to log.
			err = &maskPanicError{rec}
		}
	}()
	return MaskCallForTest(m, p), nil
}

// maskPanicError wraps a recovered panic value as an error.
type maskPanicError struct{ val any }

func (e *maskPanicError) Error() string {
	return "secretmask: recovered panic in Masker.Mask (fail-closed: chunk replaced with placeholder)"
}
