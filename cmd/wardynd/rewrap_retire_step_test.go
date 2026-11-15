// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// A Put seals its row before it writes it, so a write sealed before the latest
// version was made can land after the run: both retirement steps name the exit
// of every wardynd process that was running then, which the step depends on.
func TestRetireStep_NamesTheOldProcessesExit(t *testing.T) {
	for _, id := range []string{kek.AzureKeyIDPrefix + "v.vault.azure.net/k/s", "transit:transit/wardyn"} {
		if got := retireStep(id, "2"); !strings.Contains(got, "if every wardynd process that was running when") || !strings.Contains(got, "had exited before this run started") {
			t.Errorf("retireStep(%q) = %q; want the process exit it depends on", id, got)
		}
	}
}
