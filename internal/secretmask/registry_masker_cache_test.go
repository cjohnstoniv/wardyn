// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

// TestRegistryMaskerIsCachedPerGeneration pins F076's rebuild half: the two
// masking consumers (api's liveMaskWriter on every PTY chunk, wardynd's
// maskingRecorder on every audit event) called NewMasker(Snapshot(id)) per
// event, which clones the corpus twice and sorts it. The registry now derives
// the masker once per generation, and a registration or an eviction is what
// invalidates it.
func TestRegistryMaskerIsCachedPerGeneration(t *testing.T) {
	r := NewRegistry()
	runID := uuid.New()
	r.Add(runID, []byte("first-registered-secret-value"))

	a, b := r.Masker(runID), r.Masker(runID)
	if len(a.Secrets()) != 1 {
		t.Fatalf("Masker corpus = %d, want 1", len(a.Secrets()))
	}
	if &a.Secrets()[0][0] != &b.Secrets()[0][0] {
		t.Error("two Masker() calls with nothing registered in between rebuilt the corpus")
	}

	// A new registration must be visible immediately — a stale cache would mask
	// nothing, which is the fail-OPEN direction this layer must never take.
	r.Add(runID, []byte("second-registered-secret-value"))
	if got := r.Masker(runID).Mask([]byte("x second-registered-secret-value y")); bytes.Contains(got, []byte("second-registered")) {
		t.Errorf("a secret registered after the first Masker() was not masked: %q", got)
	}
	// So must a global, and an eviction.
	r.AddGlobal([]byte("a-late-process-global-value"))
	if got := r.Masker(runID).Mask([]byte("x a-late-process-global-value y")); bytes.Contains(got, []byte("a-late-process-global")) {
		t.Errorf("a global registered after the last Masker() was not masked: %q", got)
	}
	r.Evict(runID)
	if n := len(r.Masker(runID).Secrets()); n != 1 {
		t.Errorf("Masker corpus after Evict = %d, want 1 (the global only)", n)
	}

	// JSONVariantMasker rides the same generation and still expands.
	r2 := NewRegistry()
	id2 := uuid.New()
	r2.Add(id2, []byte("line-one\nline-two-of-a-pem-key"))
	v := r2.JSONVariantMasker(id2)
	if got := v.Mask([]byte(`{"k":"line-one\nline-two-of-a-pem-key"}`)); bytes.Contains(got, []byte("line-two-of-a-pem-key")) {
		t.Errorf("JSON-escaped variant not masked: %q", got)
	}
	if v2 := r2.JSONVariantMasker(id2); &v.Secrets()[0][0] != &v2.Secrets()[0][0] {
		t.Error("two JSONVariantMasker() calls with nothing registered in between rebuilt the corpus")
	}
}
