// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// TestRegistryAddDeduplicates pins F076's growth half. Registry.Add appended
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

	// A value already registered process-globally is not re-added per-run.
	glob := []byte("a-process-global-subscription-blob")
	r.AddGlobal(glob)
	r.Add(runID, glob)
	if n := len(r.Snapshot(runID)); n != 7 {
		t.Errorf("Snapshot after re-adding a global per-run = %d entries, want 7", n)
	}

	// The whole corpus is still masked.
	masked := NewMasker(r.Snapshot(runID)).Mask([]byte("x " + string(pat) + " y a-distinct-secret-value-03 z " + string(glob)))
	if bytes.Contains(masked, pat) || bytes.Contains(masked, glob) ||
		bytes.Contains(masked, []byte("a-distinct-secret-value-03")) {
		t.Errorf("de-duplicated corpus stopped masking a registered secret: %q", masked)
	}
}
