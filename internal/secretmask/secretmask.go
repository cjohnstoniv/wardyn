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
	"slices"
	"sort"
	"sync"
	"time"

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
	mu     sync.RWMutex
	perRun map[uuid.UUID][][]byte // run id -> set of secret values
	// globals is every process-wide value masked right now, on every run: the
	// union of current and retired, rebuilt by reflatten whenever either changes.
	globals [][]byte
	// current holds each credential's live values by (owner, name), so a
	// refreshed or deleted credential's old values can be let go (CS-4, F5)
	// instead of living for the daemon's whole life.
	current map[globalKey][][]byte
	// retired holds values that are no longer current. They stay masked until
	// SweepGlobals drops them: masking fails open, and an event quoting the old
	// value can still arrive after the credential moved on.
	retired []retiredValue

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

type globalKey struct{ owner, name string }

type retiredValue struct {
	value []byte
	at    time.Time
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
	return &Registry{perRun: make(map[uuid.UUID][][]byte), cached: map[uuid.UUID]*runMaskers{}, current: map[globalKey][][]byte{}}
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

// AddGlobal registers values as the CURRENT values of one credential, the row
// (owner, name), masked process-wide on every run. Values shorter than MinLen
// are ignored, and so are repeats: a caller re-registers the same credential on
// every dispatch and every refresh.
//
// The key is what lets the registry let go. Every value the credential held
// before and does not hold now (a refreshed access token, a rotated refresh
// token) is retired, not dropped, and SweepGlobals drops it later. Pass every
// value the credential currently holds in one call: a value left out is retired.
// A call with no usable value (every one empty or below MinLen) changes
// nothing; EvictGlobal is the one way to retire a credential's whole set.
func (r *Registry) AddGlobal(owner, name string, values ...[]byte) {
	r.setGlobal(owner, name, false, values)
}

// MergeGlobal is AddGlobal that retires nothing: values join the credential's
// current ones. It is for a caller that may hold a stale read of the
// credential (a dispatch that read the row outside the refresh's lock), which
// must never retire the values a concurrent refresh just made current. The
// paths that know the credential's full new set (capture, refresh) use
// AddGlobal.
func (r *Registry) MergeGlobal(owner, name string, values ...[]byte) {
	r.setGlobal(owner, name, true, values)
}

func (r *Registry) setGlobal(owner, name string, merge bool, values [][]byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := globalKey{owner, name}
	var keep [][]byte
	if merge {
		keep = slices.Clone(r.current[k])
	}
	for _, v := range values {
		if len(v) >= MinLen && !containsSlice(keep, v) {
			keep = append(keep, bytes.Clone(v))
		}
	}
	if len(keep) == 0 {
		return
	}
	r.retireLocked(k, keep)
	r.current[k] = keep
	// A value that comes back is current again, not waiting to be swept.
	r.retired = slices.DeleteFunc(r.retired, func(rv retiredValue) bool { return containsSlice(keep, rv.value) })
	r.reflattenLocked()
}

// EvictGlobal retires every current value of the credential (owner, name): the
// credential was deleted. The values stay masked until SweepGlobals drops them.
// Idempotent.
func (r *Registry) EvictGlobal(owner, name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retireLocked(globalKey{owner, name}, nil)
	r.reflattenLocked()
}

// SweepGlobals drops the values retired before cutoff and reports how many it
// dropped. The production caller is api.Server.SweepRunSecrets, with the same
// grace a finished run's corpus gets.
func (r *Registry) SweepGlobals(cutoff time.Time) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	before := len(r.retired)
	r.retired = slices.DeleteFunc(r.retired, func(rv retiredValue) bool { return rv.at.Before(cutoff) })
	r.reflattenLocked()
	return before - len(r.retired)
}

// retireLocked moves k's current values that are not in keep to retired and
// forgets k. The caller holds r.mu.
func (r *Registry) retireLocked(k globalKey, keep [][]byte) {
	now := time.Now()
	for _, v := range r.current[k] {
		if !containsSlice(keep, v) {
			r.retired = append(r.retired, retiredValue{value: v, at: now})
		}
	}
	delete(r.current, k)
}

// reflattenLocked rebuilds globals from current and retired, and bumps gen only
// when the masked set changed: retiring a value masks exactly what it did
// before, so it must not invalidate every run's cached Masker. The caller
// holds r.mu.
func (r *Registry) reflattenLocked() {
	seen := map[string]bool{}
	var out [][]byte
	add := func(v []byte) {
		if !seen[string(v)] {
			seen[string(v)] = true
			out = append(out, v)
		}
	}
	for _, vs := range r.current {
		for _, v := range vs {
			add(v)
		}
	}
	for _, rv := range r.retired {
		add(rv.value)
	}
	if len(out) == len(r.globals) && !slices.ContainsFunc(r.globals, func(v []byte) bool { return !seen[string(v)] }) {
		return
	}
	r.globals = out
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
	_, hadSecrets := r.perRun[runID]
	delete(r.perRun, runID)
	delete(r.cached, runID)
	// The bump is CONDITIONAL (B11b-F8). gen is the cache key for every run, so
	// an unconditional bump invalidates every OTHER run's cached Masker — and
	// once RunIDs started listing cache-only ids below, one sweep could evict N
	// of them and re-derive every live run's corpus (clone + sort) N times on
	// the masking hot path. An eviction that deleted no per-run secrets changed
	// nothing any other run's Masker was built from, so it is not a generation
	// change; this run's own cached entry is dropped above either way.
	if hadSecrets {
		r.gen++
	}
	r.mu.Unlock()
}

// RunIDs returns the run ids the registry is holding anything for: a per-run
// secret corpus, a cached Masker derived for that run, or both. It is the
// eviction lane's input (see Evict): the only way to ask what the registry is
// still holding without handing out the values themselves.
//
// The UNION, not just perRun (B11b-F8). Masker caches a derived Masker for
// every run id that asks for one, including a run with no per-run secrets at
// all — a scan run, a grantless run — whose corpus is the process globals.
// Listing perRun alone made those ids invisible to the sweep, so their cached
// clones lived for the process lifetime: the leak W12-S1-2 closed, one field
// over. Evict already deletes from both maps, so nothing else had to change.
func (r *Registry) RunIDs() []uuid.UUID {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]uuid.UUID, 0, len(r.perRun)+len(r.cached))
	for id := range r.perRun {
		out = append(out, id)
	}
	for id := range r.cached {
		if _, dup := r.perRun[id]; !dup {
			out = append(out, id)
		}
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
// Closing a borrowed stream here could hide the caller's source error behind
// a clean EOF, making a truncated recording look like a successful upload.
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
