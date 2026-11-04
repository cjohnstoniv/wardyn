// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// TestKeptMachineNewWorstBytesIsTheRunners: the list's worst_bytes is the most one new kept machine takes,
// the sum the runner checks its kept limit with.
func TestKeptMachineNewWorstBytesIsTheRunners(t *testing.T) {
	if want := admission.RunDiskReservation(macvm.BaseDiskBytes); keptMachineNewWorstBytes != want {
		t.Fatalf("keptMachineNewWorstBytes = %d, want the runner's %d", keptMachineNewWorstBytes, want)
	}
}
