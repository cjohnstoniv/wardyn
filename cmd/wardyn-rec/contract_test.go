// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestContract_PrintsTheLineAndNeedsNoRun pins what a pre-wrapped image's registration check reads: exactly
// the contract line on stdout and a nil error (exit 0), with no -run, no agent argv and no cast dir created.
func TestContract_PrintsTheLineAndNeedsNoRun(t *testing.T) {
	castDir := t.TempDir() + "/never"
	var err error
	out := captureFile(t, &os.Stdout, func() { err = run([]string{"-contract", "-cast-dir", castDir}) })
	if err != nil {
		t.Fatalf("run(-contract) = %v, want nil", err)
	}
	if want := types.ImageContractLine + "\n"; out != want {
		t.Fatalf("run(-contract) printed %q, want %q", out, want)
	}
	if _, statErr := os.Stat(castDir); !os.IsNotExist(statErr) {
		t.Fatalf("run(-contract) touched the cast dir (stat: %v)", statErr)
	}
}
