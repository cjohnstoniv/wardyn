// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRegistryAddDeduplicates pins the growth half. Registry.Add appended
// unconditionally while its sibling AddGlobal de-duplicated, and AddGlobal's own
// doc comment names exactly the hazard the per-run lane still had: "duplicates
// would grow globals without bound — Snapshot clones and NewMasker sorts the
// whole set on every masked chunk, so the masking hot path pays for each one."
// The per-run growth driver is broker.mint, which re-Adds the minted token on
// every mint with no rate limiter in internal/broker, and a leased git_pat run
// re-mints the SAME PAT on every git operation.
//
// De-duplicating cannot change WHAT is masked — masking is exact-match over a
// set — which the masking assertion at the end holds it to.
func TestRegistryAddDeduplicates(t *testing.T) {
	r := NewRegistry()
	runID := uuid.New()
	pat := []byte("ghp_theSameLeasedPatOnEveryGitOperation")

	for range 500 {
		r.Add(runID, pat)
	}
	if n := len(r.Snapshot(runID)); n != 1 {
		t.Errorf("Snapshot after 500 identical Add() = %d entries, want 1 (AddGlobal has de-duplicated all along)", n)
	}

	// Distinct values must still all be registered — dedup, not a cap.
	for i := range 5 {
		r.Add(runID, []byte(fmt.Sprintf("a-distinct-secret-value-%02d", i)))
	}
	if n := len(r.Snapshot(runID)); n != 6 {
		t.Errorf("Snapshot after 5 distinct Add() = %d entries, want 6 — dedup must not drop distinct secrets", n)
	}

	// A value also registered process-globally becomes the run's own as well,
	// and is still in the run's corpus and its maskers once: the set does not
	// double.
	glob := []byte("a-process-global-subscription-blob")
	now := time.Now()
	r.AddGlobal(0, "", "test-credential", now, glob)
	r.Add(runID, glob)
	if n := len(r.Snapshot(runID)); n != 7 {
		t.Errorf("Snapshot after re-adding a global per-run = %d entries, want 7", n)
	}
	if n, nj := len(r.Masker(runID).Secrets()), len(r.JSONVariantMasker(runID).Secrets()); n != 7 || nj != 7 {
		t.Errorf("the run's maskers hold %d and %d values, want 7 each", n, nj)
	}
	// The run's copy is its own: it stays when the process-global one is let go.
	if err := r.EvictGlobal("", "test-credential", now); err != nil {
		t.Fatal(err)
	}
	if swept := r.SweepGlobals(now.Add(time.Second)); swept != 1 {
		t.Errorf("the sweep dropped %d process-global values, want 1", swept)
	}
	if n := len(r.Snapshot(runID)); n != 7 {
		t.Errorf("Snapshot after the process-global copy was swept = %d entries, want the run's own 7", n)
	}

	// The whole corpus is still masked.
	masked := NewMasker(r.Snapshot(runID)).Mask([]byte("x " + string(pat) + " y a-distinct-secret-value-03 z " + string(glob)))
	if bytes.Contains(masked, pat) || bytes.Contains(masked, glob) ||
		bytes.Contains(masked, []byte("a-distinct-secret-value-03")) {
		t.Errorf("de-duplicated corpus stopped masking a registered secret: %q", masked)
	}
}

// A run that was handed a credential value still holds it after the credential
// lets it go, so the run keeps masking the bare value once the process-wide
// copy is swept. The process-wide value is registered FIRST: only that order
// meets a value the global set already holds. The run's copy is registered here
// (Add) or read from another replica's commit (AddLocal); the process-wide copy
// ends by replacement or by its own expiry, on the rules it always had.
func TestRegistry_ARunValueOutlivesTheGlobalCopyOfTheSameBytes(t *testing.T) {
	v := []byte(`a-credential-"handed"-to-a-run`)
	quoted, _ := json.Marshal(string(v))
	now := time.Now()
	adds := map[string]func(*Registry, uuid.UUID) error{
		"Add":      func(r *Registry, run uuid.UUID) error { return r.Add(run, v) },
		"AddLocal": func(r *Registry, run uuid.UUID) error { r.AddLocal(run, v); return nil },
	}
	// Each end registers the process-wide value, and returns how it is let go
	// and the time it is retired at.
	ends := map[string]func(*Registry) (func() error, time.Time){
		"replaced": func(r *Registry) (func() error, time.Time) {
			_ = r.AddGlobal(0, "alice", "cred", now, v)
			return func() error { return r.AddGlobal(0, "alice", "cred", now, []byte("the-replacement-value")) }, now
		},
		"expired": func(r *Registry) (func() error, time.Time) {
			_ = r.AddGlobalUntil(0, "alice", "cred", now, now.Add(time.Hour), v)
			return func() error { return nil }, now.Add(time.Hour)
		},
	}
	for addName, add := range adds {
		for endName, end := range ends {
			t.Run(addName+"/"+endName, func(t *testing.T) {
				r := NewRegistry()
				run := uuid.New()
				letGo, at := end(r)
				for range 2 {
					if err := add(r, run); err != nil {
						t.Fatal(err)
					}
				}
				if n := len(r.Snapshot(run)); n != 1 {
					t.Errorf("the run's corpus holds the value %d times, want once", n)
				}
				if err := letGo(); err != nil {
					t.Fatal(err)
				}
				if n := r.SweepGlobals(at); n != 0 || !masksValue(r, uuid.New(), string(v)) {
					t.Errorf("a sweep inside the process-wide copy's grace dropped %d values", n)
				}
				if n := r.SweepGlobals(at.Add(time.Second)); n != 1 {
					t.Fatalf("the sweep past the grace dropped %d values, want the process-wide copy", n)
				}
				if masksValue(r, uuid.New(), string(v)) {
					t.Error("a run that was never handed the value still masks it after the sweep")
				}
				if bytes.Contains(r.Masker(run).Mask([]byte("out "+string(v)+" end")), v) {
					t.Error("the run's plain output no longer masks the bare value it was handed")
				}
				if bytes.Contains(r.JSONVariantMasker(run).Mask(quoted), quoted[1:len(quoted)-1]) {
					t.Error("the run's JSON data no longer masks the bare value it was handed")
				}
				r.Evict(run)
				if masksValue(r, run, string(v)) {
					t.Error("evicting the run left its copy of the value masked")
				}
			})
		}
	}
}
