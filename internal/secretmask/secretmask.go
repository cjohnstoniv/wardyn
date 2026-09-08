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
// Stated precisely, because the wider reading is the one that bites (F155): the
// unit of protection is a RENDERING, not a credential. Registering "Bearer
// sk-abc" does not mask a bare "sk-abc" in the same buffer, and registering a
// token does not mask the base64 an Authorization: Basic header carries it in.
// It is the REGISTERING side's job to add every rendering its credential can
// appear in — see upstreamProxy.maskValues and registerHeaderCredential /
// registerBasicAuthCredential in internal/egress/proxy.
//
// Fail-CLOSED policy on masker panic: if Masker.Mask panics, the recovered
// panic is surfaced as an error AND the affected chunk is replaced with the
// placeholder instead of being forwarded verbatim. We must never emit raw,
// unmasked input bytes on a masker crash — that would leak the very secrets
// this layer exists to hide (invariant-1: the secrets path fails closed).
package secretmask

import (
	"bytes"
	"encoding/json"
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

	// gen bumps on every mutation that CHANGES the corpus (a de-duplicated Add
	// is not a change). It is the cache key below: a Masker built at generation
	// g stays valid until something is registered or evicted.
	gen uint64
	// cached holds the derived Maskers per run, keyed by the generation they
	// were built at. Building one clones and sorts the whole corpus, and the two
	// consumers do it on EVERY event they mask (a PTY chunk, an audit row), so
	// without this the masking hot path re-derives an unchanged set thousands of
	// times per run. Evict drops a run's entry with its secrets.
	cached map[uuid.UUID]*runMaskers
}

// runMaskers is one run's derived masking state at a single registry generation.
// variant (the JSONEscapedVariants expansion the audit recorder needs) is built
// lazily: it triples the set and is documented O(n^2), and the PTY lane never
// asks for it.
type runMaskers struct {
	gen         uint64
	plain       Masker
	variant     Masker
	haveVariant bool
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{perRun: make(map[uuid.UUID][][]byte), cached: map[uuid.UUID]*runMaskers{}}
}

// Add registers value as a secret for runID. Values shorter than MinLen are
// ignored. The value is copied so the caller may reuse the backing array.
// Never logs the secret value.
func (r *Registry) Add(runID uuid.UUID, value []byte) {
	if r == nil || len(value) < MinLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-registering a value already in this run's corpus (or already a global)
	// is a no-op — the same rule AddGlobal has carried since it was written, for
	// the same stated reason, which the per-run lane never got. The growth driver
	// is real: broker.mint re-Adds the minted token on every mint, and under the
	// per-run lease a git_pat run re-mints the SAME PAT on every git operation
	// with no rate limiter anywhere in internal/broker, so one run accumulated
	// one entry per git operation. Snapshot clones and NewMasker sorts the whole
	// set, and both ran on every PTY chunk and every audit event, so each
	// duplicate was paid for again on every masked byte.
	//
	// De-duplicating changes nothing about WHAT is masked: masking is exact-match
	// over a set, so a second copy of a value could never mask anything the first
	// did not. There is deliberately no CAP here — dropping a registered secret
	// past a ceiling would fail OPEN and emit it unmasked, which is the one thing
	// this package exists to prevent.
	if containsSlice(r.perRun[runID], value) || containsSlice(r.globals, value) {
		return
	}
	r.perRun[runID] = append(r.perRun[runID], bytes.Clone(value))
	r.gen++
}

// AddGlobal registers value as a process-global secret applied on every run.
// Values shorter than MinLen are ignored. Re-registering the same value is a
// no-op: per-run call sites (Bedrock SSO auth resolution, subscription inject)
// re-add the same blob on every dispatch and every preflight, and duplicates
// would grow globals without bound — Snapshot clones and NewMasker sorts the
// whole set on every masked chunk, so the masking hot path pays for each one.
func (r *Registry) AddGlobal(value []byte) {
	if r == nil || len(value) < MinLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, g := range r.globals {
		if bytes.Equal(g, value) {
			return
		}
	}
	r.globals = append(r.globals, bytes.Clone(value))
	r.gen++
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
		out = append(out, bytes.Clone(v))
	}
	for _, v := range globals {
		out = append(out, bytes.Clone(v))
	}
	return out
}

// Masker returns a Masker over runID's combined corpus (per-run + global),
// built ONCE per registry generation and shared thereafter.
//
// This is the accessor the masking hot paths use instead of
// NewMasker(Snapshot(id)). That pair clones every secret twice and sorts the
// whole set, and its callers run it per masked event — every PTY output chunk of
// an interactive attach, every audit event the recorder writes — so an unchanged
// corpus was re-derived from scratch thousands of times per run. Registrations
// are rare and masked events are not, so the derivation belongs on the write
// side of that ratio.
//
// The returned Masker is immutable by contract (NewMasker's own guarantee), which
// is what makes handing the same one to every caller safe. A nil *Registry
// returns a zero Masker, which masks nothing — the same pass-through Snapshot
// gives.
func (r *Registry) Masker(runID uuid.UUID) Masker {
	if r == nil {
		return Masker{}
	}
	r.mu.RLock()
	if c := r.cached[runID]; c != nil && c.gen == r.gen {
		m := c.plain
		r.mu.RUnlock()
		return m
	}
	r.mu.RUnlock()
	return r.rebuild(runID, false).plain
}

// JSONVariantMasker returns a Masker over runID's corpus expanded with
// JSONEscapedVariants — the set the audit recorder needs, because ev.Data is
// JSON and a secret bearing a newline or a quote (a minted ssh_key PEM) lands
// there escaped. Cached on the same generation as Masker, and built lazily: the
// expansion triples the set and is documented O(n^2), so the PTY lane never
// pays for it.
func (r *Registry) JSONVariantMasker(runID uuid.UUID) Masker {
	if r == nil {
		return Masker{}
	}
	r.mu.RLock()
	if c := r.cached[runID]; c != nil && c.gen == r.gen && c.haveVariant {
		m := c.variant
		r.mu.RUnlock()
		return m
	}
	r.mu.RUnlock()
	return r.rebuild(runID, true).variant
}

// rebuild derives runID's Maskers at the CURRENT generation and caches them.
// It re-checks under the write lock: two masked events racing on a cold cache
// would otherwise both build, and the second would overwrite a newer entry.
func (r *Registry) rebuild(runID uuid.UUID, withVariant bool) *runMaskers {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.cached[runID]
	if c == nil || c.gen != r.gen {
		c = &runMaskers{gen: r.gen, plain: NewMasker(r.snapshotLocked(runID))}
		if r.cached == nil {
			r.cached = map[uuid.UUID]*runMaskers{}
		}
		r.cached[runID] = c
	}
	if withVariant && !c.haveVariant {
		c.variant = NewMasker(JSONEscapedVariants(c.plain.secrets))
		c.haveVariant = true
	}
	return c
}

// snapshotLocked is Snapshot's body without the lock. The caller holds r.mu.
func (r *Registry) snapshotLocked(runID uuid.UUID) [][]byte {
	perRun, globals := r.perRun[runID], r.globals
	out := make([][]byte, 0, len(perRun)+len(globals))
	for _, v := range perRun {
		out = append(out, bytes.Clone(v))
	}
	for _, v := range globals {
		out = append(out, bytes.Clone(v))
	}
	return out
}

// Evict removes all per-run secrets for runID. Process-global secrets are
// unaffected. Idempotent.
//
// The production caller is api.Server.SweepRunSecrets, and it deliberately
// evicts LATE — a grace period after the run went terminal, never at the
// terminal transition itself. Every masking site takes its Snapshot lazily, at
// use time, and several of those uses come AFTER the run is terminal: the audit
// recorder masks each event's Data/Target as it is recorded (cmd/wardynd's
// maskingRecorder), including the finalize audit and any teardown_error a
// failed StopSandbox attaches, and a Snapshot that returns nothing masks
// nothing — this layer fails OPEN by design. Evicting inside finalizeRunTail
// would therefore unmask the very events most likely to quote a credential.
func (r *Registry) Evict(runID uuid.UUID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.perRun, runID)
	delete(r.cached, runID)
	r.gen++
	r.mu.Unlock()
}

// RunIDs returns the run ids currently holding per-run secrets. It is the
// eviction lane's input (see Evict): the only way to ask what the registry is
// still holding without handing out the values themselves.
func (r *Registry) RunIDs() []uuid.UUID {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]uuid.UUID, 0, len(r.perRun))
	for id := range r.perRun {
		out = append(out, id)
	}
	return out
}

// JSONEscapedVariants returns snap plus, for every secret whose JSON-string
// encoding alters its bytes, that escaped rendering as it appears INSIDE a JSON
// string value — the form a secret takes once it lands in JSON-encoded output:
// an audit event's Data field, or an asciicast "o" event body. A raw-value
// Masker misses those (a MULTI-LINE SSH key's newlines become \n; a quote
// becomes \"), so without this a special-char secret slips through unmasked on
// any JSON sink. Both JSON encoders wardyn's outputs use are covered: Go's
// default json.Marshal — HTML-escaping ON, so <>& become <… — which writes
// audit ev.Data (via mustJSON), and SetEscapeHTML(false), which is how asciinema
// (the recorder that writes cast bodies) renders them. A secret with no
// JSON-special bytes contributes no variant, and duplicates (a secret already in
// snap, or whose two escapings coincide) are dropped.
//
// This is the ONE home for the expansion both the recording-upload path and the
// audit maskingRecorder need (D31): the audit recorder previously masked the raw
// snapshot only, so a JSON-escaped secret in ev.Data was left intact.
//
// ponytail: O(n²) de-dup over the snapshot, which holds a handful of secrets per
// run; switch to a set only if a run ever registers thousands.
func JSONEscapedVariants(snap [][]byte) [][]byte {
	out := make([][]byte, len(snap), len(snap)*3)
	copy(out, snap)
	for _, s := range snap {
		for _, escapeHTML := range [...]bool{false, true} {
			if esc, ok := jsonStringEscape(s, escapeHTML); ok && !containsSlice(out, esc) {
				out = append(out, esc)
			}
		}
	}
	return out
}

// jsonStringEscape returns the bytes of s as they appear INSIDE a JSON string
// (json.Marshal's output minus the surrounding quotes). escapeHTML selects the
// encoder mode: false matches asciinema's cast bytes (`<`,`>`,`&` left literal),
// true matches Go's default json.Marshal (used for audit ev.Data). Returns
// ok=false only when encoding fails or the result is degenerate.
func jsonStringEscape(s []byte, escapeHTML bool) ([]byte, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(escapeHTML)
	if err := enc.Encode(string(s)); err != nil {
		return nil, false
	}
	b := bytes.TrimRight(buf.Bytes(), "\n") // Encoder.Encode appends a '\n'
	if len(b) < 2 {
		return nil, false
	}
	return bytes.Clone(b[1 : len(b)-1]), true // strip surrounding quotes
}

func containsSlice(set [][]byte, v []byte) bool {
	for _, s := range set {
		if bytes.Equal(s, v) {
			return true
		}
	}
	return false
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
			kept = append(kept, bytes.Clone(s))
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

// Secrets returns the masking set, longest-first, filtered to values of at least
// MinLen (exactly the set Mask applies). It exists so a caller that needs the
// corpus as well as the masking — api's liveMaskWriter, which also asks whether
// the chunk ends mid-secret — can read it off the CACHED Masker instead of
// taking a second Snapshot of the same unchanged set.
//
// The returned slices are the Masker's own. A Masker is immutable by contract,
// so callers must READ them only.
func (m Masker) Secrets() [][]byte { return m.secrets }

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
	w.tail = bytes.Clone(masked[len(masked)-tailLen:])

	if len(forward) > 0 {
		if _, werr := w.dst.Write(forward); werr != nil {
			return 0, werr
		}
	}
	return len(p), panicErr
}

// Close flushes the retained tail (after a final mask pass) to the downstream
// writer.
//
// OWNERSHIP: Close does NOT close dst. dst is borrowed, not owned — the caller
// constructed it and is the only party that knows how it must be terminated.
// Closing it here would make MaskingWriter a second closer racing the first: the
// brokered-upload path hands us an *io.PipeWriter whose close carries the copy
// error, and io.Pipe's error store is once-only, so a Close here would stamp EOF
// and silently swallow that error (a truncated upload would look like success).
func (w *MaskingWriter) Close() error {
	if len(w.tail) > 0 {
		masked, _ := safeMask(w.m, w.tail)
		if _, err := w.dst.Write(masked); err != nil {
			return err
		}
		w.tail = nil
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
