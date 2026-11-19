// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// A pin is its owner's to write and clear and reports the pin it replaced; another owner's write reaches
// nothing, an apply of a changed spec keeps it, and the table refuses a pin that is not a lowercase hex
// SHA-256.
func TestPG_WorkloadPolicyPin_SetReportsThePreviousAndOutlivesAnApply(t *testing.T) {
	st := store.NewPG(runsPGPoolIsolated(t))
	ctx := t.Context()
	w := mustPutWorkload(t, st, workloadPut("sub-a", "web", "d1"))
	pin := strings.Repeat("ab", 32)

	got, prev, err := st.SetWorkloadPolicyPin(ctx, "sub-a", "web", pin)
	if err != nil || prev != "" || got.PolicyPin != pin || got.ID != w.ID || got.Generation != w.Generation {
		t.Fatalf("first pin = %+v, %q, %v; want %q set over none, the spec untouched", got, prev, err, pin)
	}
	if _, _, err := st.SetWorkloadPolicyPin(ctx, "sub-b", "web", strings.Repeat("0", 64)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another owner's pin err = %v, want ErrNotFound", err)
	}
	if _, _, err := st.SetWorkloadPolicyPin(ctx, "sub-a", "web", strings.ToUpper(pin)); err == nil {
		t.Fatal("an upper-case pin was stored; want the table's check to refuse it")
	}
	changed := mustPutWorkload(t, st, workloadPut("sub-a", "web", "d2"))
	if changed.Generation != w.Generation+1 || changed.PolicyPin != pin {
		t.Fatalf("after a changed apply: generation %d, pin %q; want %d and %q kept", changed.Generation, changed.PolicyPin, w.Generation+1, pin)
	}
	cleared, prev, err := st.SetWorkloadPolicyPin(ctx, "sub-a", "web", "")
	if err != nil || prev != pin || cleared.PolicyPin != "" {
		t.Fatalf("clear = %+v, %q, %v; want the pin cleared, reporting %q", cleared, prev, err, pin)
	}
	if v, err := st.GetWorkload(ctx, "sub-a", "web"); err != nil || v.PolicyPin != "" || v.Generation != changed.Generation {
		t.Fatalf("read after clear = pin %q generation %d, %v", v.PolicyPin, v.Generation, err)
	}
}
