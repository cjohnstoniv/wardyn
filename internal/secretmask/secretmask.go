// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretmask implements Wardyn's output-masking layer for PTY capture
// and asciicast streams: it replaces verbatim byte-identical occurrences of
// registered secret values with the literal placeholder "<secret-hidden>".
//
// HONEST RESIDUAL: masking catches verbatim leakage only — base64/hex/encoded
// or model-narrated representations are NOT caught. The unit of protection is
// a RENDERING, not a credential: registering "Bearer sk-abc" doesn't mask a
// bare "sk-abc", so it's the REGISTERING side's job to add every rendering a
// credential can appear in (see upstreamProxy.maskValues).
//
// SECURITY (fail-closed): a recovered masker panic replaces the affected
// chunk with the placeholder instead of forwarding it verbatim — this layer
// must never emit raw, unmasked input bytes on a crash.
//
// SHARED CORPUS: wardynd gives its Registry a Backend (package maskstore) that
// commits every registration to Postgres before it returns, so every replica
// masks the same corpus and a restart loses none of it. The maps below are then
// that process's cache. A consumer calls Fresh before it masks a chunk and fails
// closed when it cannot prove the cache current. The egress proxy's own Registry
// has no Backend and is process-local.
package secretmask

import (
	"bytes"
	"context"
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
	// refreshed or deleted credential's old values can be let go instead of
	// living for the daemon's whole life.
	current map[globalKey][]globalValue
	// retired holds values that are no longer current. They stay masked until
	// SweepGlobals drops them: masking fails open, and an event quoting the old
	// value can still arrive after the credential moved on.
	retired []retiredValue

	// gen bumps on every mutation that CHANGES the corpus (a de-duplicated Add
	// is not a change). It is the cache key below: a Masker built at generation
	// g stays valid until something is registered or evicted.
	gen uint64
	// cached holds the derived Maskers per run, keyed by generation: building
	// one clones and sorts the whole corpus, and both masking hot-path
	// consumers do that per event, so this avoids re-deriving an unchanged set
	// thousands of times per run. Evict drops a run's entry with its secrets.
	cached map[uuid.UUID]*runMaskers

	// backend, when set, is the committed corpus every replica shares (see
	// Backend). The maps above are then this process's cache of it.
	backend Backend
}

type globalKey struct{ owner, name string }

// globalValue is one current value of a credential. until, when set, is the
// value's own expiry: from then on SweepGlobals treats it as retired at
// until, so a credential nobody refreshes again still lets go of it.
type globalValue struct {
	value []byte
	until time.Time
}

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
	return &Registry{perRun: make(map[uuid.UUID][][]byte), cached: map[uuid.UUID]*runMaskers{}, current: map[globalKey][]globalValue{}}
}

// Add registers value as a secret for runID. Values shorter than MinLen are
// ignored. The value is copied so the caller may reuse the backing array.
// Never logs the secret value.
//
// With a Backend the value is committed to it before Add returns, and an error
// means it is NOT on record: the caller must not hand the credential out.
func (r *Registry) Add(runID uuid.UUID, value []byte) error {
	if r == nil || len(value) < MinLen {
		return nil
	}
	// Re-registering an already-known value (per-run or global) is a no-op:
	// masking is exact-match over a set, so a duplicate could never mask
	// anything new. There is deliberately no CAP here: dropping a registered
	// secret past a ceiling would fail OPEN and emit it unmasked, the one
	// thing this package exists to prevent.
	r.mu.RLock()
	known := containsSlice(r.perRun[runID], value) || containsSlice(r.globals, value)
	b := r.backend
	r.mu.RUnlock()
	if known {
		return nil
	}
	// Commit first, cache second: a failed commit must leave the value unknown
	// here, or the retry would see it as known and never persist it.
	if b != nil {
		if err := b.PutRun(runID, value); err != nil {
			return err
		}
	}
	r.AddLocal(runID, value)
	return nil
}

// AddLocal is Add into this process's cache only, for a value that is already
// committed elsewhere (a masking manifest's renderings, a row another replica
// wrote). It never fails.
func (r *Registry) AddLocal(runID uuid.UUID, value []byte) {
	if r == nil || len(value) < MinLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if containsSlice(r.perRun[runID], value) || containsSlice(r.globals, value) {
		return
	}
	r.perRun[runID] = append(r.perRun[runID], bytes.Clone(value))
	r.gen++
}

// AddGlobal registers values as the CURRENT values of one credential (owner,
// name), masked process-wide on every run; a value left out of this call is
// retired (not dropped — SweepGlobals drops it later, keyed off now).
// Repeats and values shorter than MinLen are ignored.
func (r *Registry) AddGlobal(owner, name string, now time.Time, values ...[]byte) error {
	return r.setGlobal(owner, name, now, false, time.Time{}, nil, values)
}

// AddGlobalUntil is AddGlobal for a credential with one expiring value (a
// short-lived access token): it is let go once SweepGlobals sees until past
// its grace, even if nothing replaces it. Lasting values carry no expiry.
func (r *Registry) AddGlobalUntil(owner, name string, now, until time.Time, expiring []byte, lasting ...[]byte) error {
	return r.setGlobal(owner, name, now, false, until, expiring, lasting)
}

// MergeGlobal is AddGlobal that retires nothing — for a caller holding a
// possibly-stale read of the credential, which must not retire values a
// concurrent refresh just made current.
func (r *Registry) MergeGlobal(owner, name string, values ...[]byte) error {
	return r.setGlobal(owner, name, time.Time{}, true, time.Time{}, nil, values)
}

// MergeGlobalUntil is MergeGlobal with AddGlobalUntil's expiring value. An
// expiring value already current keeps the later of its two expiries.
func (r *Registry) MergeGlobalUntil(owner, name string, until time.Time, expiring []byte, lasting ...[]byte) error {
	return r.setGlobal(owner, name, time.Time{}, true, until, expiring, lasting)
}

// setGlobal's now is unused on a merge, which retires nothing. With a Backend
// the change is committed to it first, and an error means it is not on record.
func (r *Registry) setGlobal(owner, name string, now time.Time, merge bool, until time.Time, expiring []byte, lasting [][]byte) error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	b := r.backend
	r.mu.RUnlock()
	if b != nil {
		var puts []GlobalPut
		if len(expiring) >= MinLen {
			puts = append(puts, GlobalPut{Value: expiring, Until: until})
		}
		for _, v := range lasting {
			if len(v) >= MinLen {
				puts = append(puts, GlobalPut{Value: v})
			}
		}
		if len(puts) > 0 {
			if err := b.PutGlobal(owner, name, puts, merge, now); err != nil {
				return err
			}
		}
	}
	r.setGlobalLocal(owner, name, now, merge, until, expiring, lasting)
	return nil
}

// setGlobalLocal is setGlobal into this process's cache only.
func (r *Registry) setGlobalLocal(owner, name string, now time.Time, merge bool, until time.Time, expiring []byte, lasting [][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := globalKey{owner, name}
	var keep []globalValue
	if merge {
		keep = slices.Clone(r.current[k])
	}
	add := func(v []byte, until time.Time) {
		if len(v) < MinLen {
			return
		}
		i := slices.IndexFunc(keep, func(gv globalValue) bool { return bytes.Equal(gv.value, v) })
		switch {
		case i < 0:
			keep = append(keep, globalValue{value: bytes.Clone(v), until: until})
		case merge:
			keep[i].until = laterExpiry(keep[i].until, until)
		default:
			keep[i].until = until
		}
	}
	add(expiring, until)
	for _, v := range lasting {
		add(v, time.Time{})
	}
	if len(keep) == 0 {
		return
	}
	if !merge {
		r.retireLocked(k, keep, now)
	}
	r.current[k] = keep
	// A value that comes back is current again, not waiting to be swept.
	r.retired = slices.DeleteFunc(r.retired, func(rv retiredValue) bool { return containsValue(keep, rv.value) })
	r.reflattenLocked()
}

// laterExpiry returns the later of two expiries, where zero means none.
func laterExpiry(a, b time.Time) time.Time {
	if a.IsZero() || b.IsZero() {
		return time.Time{}
	}
	if a.After(b) {
		return a
	}
	return b
}

func containsValue(set []globalValue, v []byte) bool {
	return slices.ContainsFunc(set, func(gv globalValue) bool { return bytes.Equal(gv.value, v) })
}

// EvictGlobal retires every current value of the credential (owner, name): the
// credential was deleted. The values stay masked until SweepGlobals drops them.
// Idempotent. now is read as on AddGlobal. With a Backend the committed rows
// are tombstoned first, and an error means they are still there.
func (r *Registry) EvictGlobal(owner, name string, now time.Time) error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	b := r.backend
	r.mu.RUnlock()
	if b != nil {
		if err := b.EvictGlobal(owner, name, now); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retireLocked(globalKey{owner, name}, nil, now)
	r.reflattenLocked()
	return nil
}

// RetireOwnerGlobals retires every current credential value of owner, as
// EvictGlobal does for one credential: the values stay masked until
// SweepGlobals drops them. Idempotent. With a Backend the committed rows are
// retired first, and an error means they are still current.
func (r *Registry) RetireOwnerGlobals(ctx context.Context, owner string, now time.Time) error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	b := r.backend
	r.mu.RUnlock()
	if b != nil {
		if err := b.RetireOwnerGlobals(ctx, owner, now); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.current {
		if k.owner == owner {
			r.retireLocked(k, nil, now)
		}
	}
	r.reflattenLocked()
	return nil
}

// SweepGlobals drops the values retired before cutoff, and the current values
// whose expiry (AddGlobalUntil) is before cutoff, and reports how many it
// dropped. The production caller is api.Server.SweepRunSecrets, with the same
// grace a finished run's corpus gets.
func (r *Registry) SweepGlobals(cutoff time.Time) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	dropped := len(r.retired)
	r.retired = slices.DeleteFunc(r.retired, func(rv retiredValue) bool { return rv.at.Before(cutoff) })
	dropped -= len(r.retired)
	for k, vs := range r.current {
		n := len(vs)
		vs = slices.DeleteFunc(vs, func(gv globalValue) bool { return !gv.until.IsZero() && gv.until.Before(cutoff) })
		dropped += n - len(vs)
		if len(vs) == 0 {
			delete(r.current, k)
		} else {
			r.current[k] = vs
		}
	}
	r.reflattenLocked()
	return dropped
}

// retireLocked moves k's current values that are not in keep to retired and
// forgets k. A value with an expiry is retired at the later of now and that
// expiry: replacing an access token does not end it, and a cached copy may
// still be served until it expires. The caller holds r.mu.
func (r *Registry) retireLocked(k globalKey, keep []globalValue, now time.Time) {
	for _, gv := range r.current[k] {
		if !containsValue(keep, gv.value) {
			at := now
			if gv.until.After(at) {
				at = gv.until
			}
			r.retired = append(r.retired, retiredValue{value: gv.value, at: at})
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
		for _, gv := range vs {
			add(gv.value)
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
// built ONCE per registry generation and shared thereafter — the accessor
// masking hot paths use instead of NewMasker(Snapshot(id)), which clones and
// sorts the whole set on every masked event.
//
// The returned Masker is immutable by contract, so sharing it across callers
// is safe. A nil *Registry returns a zero Masker (masks nothing).
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
// JSONEscapedVariants, the set the audit recorder needs since ev.Data is JSON
// and a secret bearing a newline or quote lands there escaped. Cached on the
// same generation as Masker, built lazily since the expansion triples the
// set and is O(n^2), so the PTY lane never pays for it.
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

// rebuild derives runID's Maskers at the CURRENT generation and caches them,
// re-checking under the write lock: two masked events racing on a cold cache
// would otherwise both build, the second overwriting a newer entry.
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

// Evict removes all per-run secrets for runID from this process's cache
// (process-global secrets unaffected). Idempotent. It touches no committed row:
// with a Backend those are the retention pass's (PurgeRuns), so one replica's
// sweep never strips another's corpus.
//
// The production caller evicts LATE, a grace period after the run goes
// terminal: masking sites Snapshot lazily at use time, and the audit
// recorder's finalize event happens after terminal. SECURITY: since this
// layer fails OPEN by design, evicting early would unmask exactly that event.
func (r *Registry) Evict(runID uuid.UUID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	_, hadSecrets := r.perRun[runID]
	delete(r.perRun, runID)
	delete(r.cached, runID)
	// The bump is CONDITIONAL: gen is the cache key for every run, so an
	// unconditional bump would invalidate every OTHER run's cached Masker on
	// every eviction. An eviction that deleted no per-run secrets changed
	// nothing any other run's Masker was built from.
	if hadSecrets {
		r.gen++
	}
	r.mu.Unlock()
}

// RunIDs returns the run ids the registry holds anything for (a per-run
// corpus, a cached Masker, or both) — the eviction lane's input.
//
// The UNION, not just perRun: Masker caches an entry even for a run with no
// per-run secrets (its corpus is the process globals), so perRun alone would
// hide those ids from the sweep and leak their cached clones for the process
// lifetime.
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

// JSONEscapedVariants returns snap plus, for each secret, its JSON-escaped
// rendering as it appears INSIDE a JSON string value (an audit event's Data
// field, an asciicast "o" event body) — a raw-value Masker otherwise misses
// a multi-line SSH key's escaped newlines. Covers both JSON encoders wardyn
// uses: Go's default json.Marshal (HTML-escaping on) and SetEscapeHTML(false)
// (asciinema). No variant for a secret with no JSON-special bytes;
// duplicates dropped.
//
// ponytail: O(n²) de-dup over the snapshot (a handful of secrets per run);
// switch to a set only if a run ever registers thousands.
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
// (json.Marshal's output minus surrounding quotes). escapeHTML selects the
// encoder mode: false matches asciinema's cast bytes, true matches Go's
// default json.Marshal. ok=false only on encode failure or a degenerate result.
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

// Secrets returns the masking set, longest-first, filtered to values of at
// least MinLen (exactly the set Mask applies), for a caller needing the
// corpus as well as the masking to read it off the cached Masker instead of
// taking a second Snapshot.
//
// The returned slices are the Masker's own; callers must READ them only.
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
// stream, even when a secret spans two adjacent Write calls (chunked PTY or
// asciicast frames).
//
// Invariant: after each Write, it retains a tail of (maxSecretLen-1) bytes
// from the masked buffer, prepended to the next chunk before masking again.
// Close() flushes the retained tail.
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
// so the caller can record the anomaly.
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

// Close flushes the retained tail (after a final mask pass) to dst.
//
// OWNERSHIP: Close does NOT close dst — dst is borrowed, not owned. Closing a
// borrowed stream here could hide the caller's source error behind a clean
// EOF, making a truncated recording look like a successful upload.
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

// safeMask wraps Mask with panic recovery. On a recovered panic it FAILS
// CLOSED: returns the placeholder (not the original bytes) plus an error, so
// no unmasked secret can leak downstream and the caller can log the anomaly.
func safeMask(m Masker, p []byte) (out []byte, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			// Fail-CLOSED: substitute the placeholder so raw input bytes are
			// never written.
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
