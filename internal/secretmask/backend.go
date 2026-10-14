// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
)

// ErrErased refuses credential work that began before its owner's mask erase.
var ErrErased = errors.New("masking values were erased while the credential was being obtained")

// GlobalPut is one value of a credential handed to Backend.PutGlobal. A zero
// Until is a value that does not expire on its own.
type GlobalPut struct {
	Value []byte
	Until time.Time
}

// Backend is the committed corpus a Registry shares with other processes. The
// Registry's own maps are then this process's cache of it: a mutator commits to
// the Backend first and returns its error, so a value is on record before any
// caller can hand the credential out, and a Backend that cannot answer fails
// the registration instead of passing it silently.
//
// A nil Backend keeps the Registry process-local (the egress proxy's, and the
// tests').
type Backend interface {
	// PutRun commits value as run runID's. A run with no durable owner (one
	// dispatched before masking manifests) is kept in this process only, and
	// that is not an error.
	PutRun(runID uuid.UUID, value []byte) error
	// GlobalGeneration snapshots the committed cursor before credential work begins.
	GlobalGeneration(ctx context.Context) (int64, error)
	// PutGlobal commits values as credential (owner, name)'s. merge retires
	// nothing; otherwise the credential's other current values are retired.
	// It refuses an owner erased after generation and succeeds only after the
	// committed rows are applied and the requested values are available to mask.
	PutGlobal(generation int64, owner, name string, values []GlobalPut, merge bool, now time.Time) error
	// EvictGlobal tombstones the credential's current values.
	EvictGlobal(owner, name string, now time.Time) error
	// RetireOwnerGlobals retires every current credential value of owner.
	RetireOwnerGlobals(ctx context.Context, owner string, now time.Time) error
	// SweepGlobals tombstones the credential values retired, or expired, before
	// cutoff, and reports how many.
	SweepGlobals(ctx context.Context, cutoff time.Time) (int, error)
	// PersistedRuns lists the runs that have a committed masking manifest or
	// per-run value: the retention pass's candidates.
	PersistedRuns(ctx context.Context) ([]uuid.UUID, error)
	// PurgeRuns tombstones the per-run values of runs, and deletes their masking
	// manifests.
	PurgeRuns(ctx context.Context, runs []uuid.UUID) error
	// EraseOwner advances the durable owner fence, tombstones every value, and reports how
	// many rows still hold ciphertext for it afterwards (zero when it is done).
	EraseOwner(ctx context.Context, owner string) (remaining int, err error)
	// Fresh returns nil only when this process's cache holds every value
	// committed before a read of the corpus that began after arrived.
	Fresh(ctx context.Context, arrived time.Time) error
}

// GlobalGeneration snapshots erasure ordering before any credential read or
// network exchange. The owner's identity may be established by that exchange.
func (r *Registry) GlobalGeneration(ctx context.Context) (int64, error) {
	if b := r.backendOf(); b != nil {
		return b.GlobalGeneration(ctx)
	}
	return 0, nil
}

// SetBackend attaches the shared corpus. It is set once, at boot, before the
// Registry is used.
func (r *Registry) SetBackend(b Backend) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.backend = b
	r.mu.Unlock()
}

// freshTimeout bounds one Fresh: a Postgres that does not answer is "not fresh",
// never a hang.
const freshTimeout = 5 * time.Second

// Fresh reports whether this process holds the whole committed corpus as of a
// read that began after arrived (the moment the bytes about to be masked
// arrived). A Registry with no Backend is its own corpus and is always fresh.
// False means the caller cannot vouch for its masking and must fail closed: a
// placeholder for a chunk, a 503 for an upload, a refused attach.
func (r *Registry) Fresh(arrived time.Time) bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	b := r.backend
	r.mu.RUnlock()
	if b == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), freshTimeout)
	defer cancel()
	return b.Fresh(ctx, arrived) == nil
}

// Persisted reports whether this Registry has a Backend.
func (r *Registry) Persisted() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.backend != nil
}

// Generation is the cache key every Masker is derived at: it moves whenever the
// masked set changes.
func (r *Registry) Generation() uint64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.gen
}

// HasGlobalValues reports whether every value of a committed credential is
// present, including copies retained through retirement grace.
func (r *Registry) HasGlobalValues(owner, name string, values []GlobalPut) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	k := globalKey{owner, name}
	current := r.current[k]
	for _, put := range values {
		if containsValue(current, put.Value) || slices.ContainsFunc(r.retired, func(v retiredValue) bool {
			return bytes.Equal(v.value, put.Value)
		}) {
			continue
		}
		held := false
		for _, row := range r.rows {
			if bytes.Equal(row.value, put.Value) && (row.key == k || !row.retiredAt.IsZero()) {
				held = true
				break
			}
		}
		if !held {
			return false
		}
	}
	return true
}

// SweepPersisted is SweepGlobals for the committed rows: it tombstones the
// credential values retired or expired before cutoff. The leader pass calls it;
// a Registry with no Backend has nothing to sweep.
func (r *Registry) SweepPersisted(ctx context.Context, cutoff time.Time) (int, error) {
	if b := r.backendOf(); b != nil {
		return b.SweepGlobals(ctx, cutoff)
	}
	return 0, nil
}

// PersistedRuns lists the runs that have committed masking state.
func (r *Registry) PersistedRuns(ctx context.Context) ([]uuid.UUID, error) {
	if b := r.backendOf(); b != nil {
		return b.PersistedRuns(ctx)
	}
	return nil, nil
}

// PurgeRuns tombstones the committed per-run values of runs and deletes their
// masking manifests: the retention pass for runs terminal past their grace.
func (r *Registry) PurgeRuns(ctx context.Context, runs []uuid.UUID) error {
	if b := r.backendOf(); b != nil && len(runs) > 0 {
		return b.PurgeRuns(ctx, runs)
	}
	return nil
}

// EraseOwner tombstones every committed value under owner (per-run and global)
// and reports how many rows still hold ciphertext for it. Zero is done.
func (r *Registry) EraseOwner(ctx context.Context, owner string) (int, error) {
	if b := r.backendOf(); b != nil {
		return b.EraseOwner(ctx, owner)
	}
	return 0, nil
}

func (r *Registry) backendOf() Backend {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.backend
}

// Placeholder is the literal bytes a masked value, or a chunk that cannot be
// vouched for, is replaced by.
func Placeholder() []byte { return bytes.Clone(placeholder) }

// ─── applying what another replica committed ────────────────────────────────

// ApplyGlobal puts committed row id's credential value into this process's
// cache: current (retiredAt zero, expiring at until if that is set) or retired
// at retiredAt. It is the row's own state and changes no other row's, so a row
// retired or dropped later never ends what another row of the same bytes still
// holds. It touches no committed row.
func (r *Registry) ApplyGlobal(id uuid.UUID, owner, name string, value []byte, until, retiredAt time.Time) {
	if r == nil || len(value) < MinLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.rows[id]; ok && bytes.Equal(old.value, value) {
		value = old.value
	} else {
		value = bytes.Clone(value)
	}
	if r.rows == nil {
		r.rows = map[uuid.UUID]globalRow{}
	}
	r.rows[id] = globalRow{key: globalKey{owner, name}, value: value, until: until, retiredAt: retiredAt}
	r.reflattenLocked()
}

// RetireGlobalValue retires committed row id's current value at at, where it
// stays masked until SweepGlobals drops it. A row not current here is left as
// it is.
func (r *Registry) RetireGlobalValue(id uuid.UUID, at time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if row, ok := r.rows[id]; ok && row.retiredAt.IsZero() {
		row.retiredAt = at
		r.rows[id] = row
	}
}

// DropGlobalValue forgets committed row id's value, current or retired.
func (r *Registry) DropGlobalValue(id uuid.UUID) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rows, id)
	r.reflattenLocked()
}

// DropRunValue forgets one value of runID's per-run corpus.
func (r *Registry) DropRunValue(runID uuid.UUID, value []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	vs := r.perRun[runID]
	n := len(vs)
	vs = slices.DeleteFunc(vs, func(v []byte) bool { return bytes.Equal(v, value) })
	if len(vs) == n {
		return
	}
	if len(vs) == 0 {
		delete(r.perRun, runID)
	} else {
		r.perRun[runID] = vs
	}
	delete(r.cached, runID)
	r.gen++
}
