// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"sync"
	"testing"
)

// Of callers racing to clear one ref, one answers true; a ref that is not the run's is left alone.
func TestPG_ClearSandboxRefRetainingIf_OnlyTheMatchingRefClearsOnce(t *testing.T) {
	f := newActionFixture(t)
	run := f.run(t)
	if cleared, err := f.st.ClearSandboxRefRetainingIf(t.Context(), run.ID, "another-ref"); err != nil || cleared {
		t.Fatalf("clear of another ref = %v, %v; want false", cleared, err)
	}
	if got, err := f.st.GetRun(t.Context(), run.ID); err != nil || got.SandboxRef != run.SandboxRef {
		t.Fatalf("ref after the refused clear = %q, %v; want %q", got.SandboxRef, err, run.SandboxRef)
	}
	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.st.ClearSandboxRefRetainingIf(t.Context(), run.ID, run.SandboxRef)
		}()
	}
	wg.Wait()
	cleared := 0
	for i := range results {
		if errs[i] != nil {
			t.Errorf("clear %d: %v", i, errs[i])
		}
		if results[i] {
			cleared++
		}
	}
	if cleared != 1 {
		t.Errorf("%d clears answered true, want 1", cleared)
	}
	if got, err := f.st.GetRun(t.Context(), run.ID); err != nil || got.SandboxRef != "" {
		t.Errorf("ref after the clear = %q, %v; want empty", got.SandboxRef, err)
	}
}
