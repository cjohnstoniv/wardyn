// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// approvalMethodRE matches the declaration line of an approvalService method.
var approvalMethodRE = regexp.MustCompile(`(?m)^func \(s \*approvalService\) .*\{$`)

// The harness copies the daemon's approval adapter (harness_test.go says verbatim); cmd/wardynd is
// package main and cannot be imported, so this reads the source and pins that the two declare the
// same methods. A capability added there and not here would send the e2e server down the
// handlers' fallback path instead of the one the daemon runs.
func TestApprovalServiceHasTheDaemonsMethods(t *testing.T) {
	methods := func(path string) []string {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		m := approvalMethodRE.FindAllString(string(src), -1)
		slices.Sort(m)
		return m
	}
	daemon, harness := methods("../../cmd/wardynd/adapters.go"), methods("harness_test.go")
	if len(daemon) == 0 {
		t.Fatal("found no approvalService methods in cmd/wardynd/adapters.go: the pattern or the file moved")
	}
	if !slices.Equal(daemon, harness) {
		t.Errorf("approvalService methods differ.\ndaemon:  %q\nharness: %q", daemon, harness)
	}
}
