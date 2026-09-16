// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import (
	"testing"

	"github.com/google/uuid"
)

// TestEvict_GenerationBumpsOnlyOnARealDelete pins the verifier's correction to
// B11b-F8. The finding's own fix — make RunIDs return the cache-only ids too —
// is only safe alongside this one: Evict bumped gen UNCONDITIONALLY, and gen is
// the cache key for EVERY run, so a sweep that newly evicts N cache-only ids
// would invalidate every live run's cached Masker N times and re-derive it
// (clone + sort the whole corpus) on the next masked byte. A generation storm
// on the masking hot path, caused by a fix for a memory leak.
//
// The bump belongs where the corpus actually changed: an eviction that deletes
// no per-run secrets changed nothing any other run's Masker was built from.
func TestEvict_GenerationBumpsOnlyOnARealDelete(t *testing.T) {
	r := NewRegistry()
	live := uuid.New()
	r.Add(live, []byte("live-secret-value"))
	_ = r.Masker(live)

	r.mu.RLock()
	before := r.cached[live].gen
	r.mu.RUnlock()

	for i := 0; i < 8; i++ {
		r.Evict(uuid.New()) // ids that never held a per-run secret
	}

	r.mu.RLock()
	cur, gen := r.cached[live], r.gen
	r.mu.RUnlock()
	if cur == nil || cur.gen != before || gen != before {
		t.Fatalf("cache-only evictions moved the generation (cached=%v, registry gen=%d, want %d) — "+
			"every live run's Masker would be re-derived", cur, gen, before)
	}

	// A real per-run eviction still bumps, because it must: the corpus the
	// cached Masker was built from is gone.
	r.Evict(live)
	r.mu.RLock()
	after := r.gen
	r.mu.RUnlock()
	if after == before {
		t.Error("a real eviction must bump the generation")
	}
	if len(r.Snapshot(live)) != 0 {
		t.Error("a real eviction must drop the run's per-run secrets")
	}
}

// TestRunIDs_IncludesCacheOnlyRuns is B11b-F8 itself: the eviction lane's only
// input is RunIDs, and it listed the per-run map alone — so a run that never
// registered a secret but DID have a Masker derived for it (a scan run, a
// grantless run) was invisible to the sweep and its cached corpus lived for the
// process lifetime.
func TestRunIDs_IncludesCacheOnlyRuns(t *testing.T) {
	r := NewRegistry()
	r.AddGlobal([]byte("a-process-global-secret"))
	withSecret, cacheOnly := uuid.New(), uuid.New()
	r.Add(withSecret, []byte("per-run-secret-value"))
	_ = r.Masker(cacheOnly)

	ids := map[uuid.UUID]bool{}
	for _, id := range r.RunIDs() {
		ids[id] = true
	}
	if !ids[withSecret] {
		t.Error("RunIDs dropped a run holding per-run secrets")
	}
	if !ids[cacheOnly] {
		t.Error("RunIDs must also list a run the registry holds only a CACHED Masker for — " +
			"nothing else ever asks the registry what it is still holding")
	}
}
