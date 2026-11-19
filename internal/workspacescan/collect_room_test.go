// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A service or a host the scan sees many times takes one place in what it collects: one seen after
// the repeats is still kept. A host past the collection room marks the scan Truncated.
func TestScan_RepeatsDoNotCrowdOutALaterServiceOrHost(t *testing.T) {
	root := t.TempDir()
	var compose, gradle strings.Builder
	compose.WriteString("services:\n")
	for i := 0; i < maxServices*collectSlack; i++ {
		fmt.Fprintf(&compose, "  db%d:\n    image: postgres:16\n", i)
	}
	compose.WriteString("  cache:\n    image: redis:7\n")
	writeFile(t, root, "docker-compose.yml", compose.String())
	gradle.WriteString("repositories {\n")
	for i := 0; i < maxSuggested*collectSlack; i++ {
		gradle.WriteString("  maven { url \"https://repo.one.example.com/m2\" }\n")
	}
	gradle.WriteString("  maven { url \"https://late.two.example.com/m2\" }\n}\n")
	writeFile(t, root, "build.gradle", gradle.String())

	got := Scan(root)
	if !slices.Contains(got.ServicesNeeded, "redis") || !slices.Contains(got.ServicesNeeded, "postgres") {
		t.Errorf("services = %v, want postgres and the later redis", got.ServicesNeeded)
	}
	if !slices.Contains(got.SuggestedEgress, "late.two.example.com") {
		t.Errorf("suggested hosts = %v, want the later late.two.example.com", got.SuggestedEgress)
	}
	if got.Incomplete {
		t.Error("repeats alone marked the scan incomplete")
	}

	var facts ScanFacts
	for i := 0; i <= maxSuggested*collectSlack; i++ {
		addSuggestedHost(&facts, fmt.Sprintf("h%d.example.com", i))
	}
	if len(facts.SuggestedEgress) != maxSuggested*collectSlack || !facts.Truncated {
		t.Errorf("%d hosts kept, truncated %v; want %d and true", len(facts.SuggestedEgress), facts.Truncated, maxSuggested*collectSlack)
	}
}
