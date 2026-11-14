// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// The status line's held disk is the debts summed through admission.SumDisk: two ended logins' VMs whose
// reservations together pass the largest int64 show the largest figure, never a wrapped one (a negative
// sum reads as no held disk at all). Catches status() summing the debts with plain addition.
func TestMacOSVMStatusHeldDiskHoldsAtTheLargest(t *testing.T) {
	r := newMacLedgerRig(t)
	r.host.setProcs(
		admission.Proc{PID: 90, UID: 502, Start: 9, Name: admission.HelperProcess},
		admission.Proc{PID: 91, UID: 502, Start: 9, Name: admission.HelperProcess})
	m := r.open(t)
	m.helper, _ = fakeHelper(t, 0)
	first, second := r.otherRun(t, 90), r.otherRun(t, 91)
	// Planted after both are admitted: admission itself would refuse the second past the first's figure.
	for _, e := range []admission.Entry{first, second} {
		file := filepath.Join(r.dir, "e-"+e.ID+".json")
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		fields["disk_reserve_bytes"] = int64(math.MaxInt64/2 + 1)
		if body, err = json.Marshal(fields); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r.host.setProcs() // both logins' helpers are gone, so the next pass tombstones their entries
	st, err := m.status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.HeldDiskBytes != math.MaxInt64 {
		t.Fatalf("held disk = %d, want %d: the two debts together pass the largest int64", st.HeldDiskBytes, int64(math.MaxInt64))
	}
}
