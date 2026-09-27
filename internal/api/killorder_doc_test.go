// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"regexp"
	"testing"
)

// TestKillOrderDocs_MatchC002CASFirst: threatmodel/THREAT-MODEL.md and
// pkg/client/client.go's KillRun doc comment must state the CAS-first kill
// order for the explicit-kill path (handleKillRun/runs_lifecycle.go, see "Win
// the terminal transition first"), not sandbox teardown before the state CAS
// — with teardown first, a kill that lost a race to a concurrent
// forward-transition could revoke credentials out from under a run it no
// longer owned.
func TestKillOrderDocs_MatchC002CASFirst(t *testing.T) {
	tm, err := os.ReadFile("../../threatmodel/THREAT-MODEL.md")
	if err != nil {
		t.Fatalf("read threatmodel/THREAT-MODEL.md: %v", err)
	}
	tmDoc := string(tm)

	// The numbered "explicit kill" cascade must list the durable state
	// transition (CAS) as step 1, not teardown.
	m := regexp.MustCompile(`(?s)explicit kill\*\* path \(\x60handleKillRun\x60\) runs this fixed order:\n\n1\. \*\*([^*]+)\*\*`).FindStringSubmatch(tmDoc)
	if m == nil {
		t.Fatal(`threatmodel/THREAT-MODEL.md: could not find the explicit-kill numbered cascade — update this guard's anchor if the section was reworded`)
	}
	if !regexp.MustCompile(`(?i)state transition|compare-and-swap|CAS`).MatchString(m[1]) {
		t.Errorf("threatmodel/THREAT-MODEL.md's explicit-kill cascade step 1 is %q, want the CAS/state-transition step (C002 made it first, not last)", m[1])
	}
	if regexp.MustCompile(`(?i)teardown-first`).MatchString(tmDoc) {
		t.Error(`threatmodel/THREAT-MODEL.md still describes the explicit-kill path as "teardown-first" — that's the pre-C002 order`)
	}

	client, err := os.ReadFile("../../pkg/client/client.go")
	if err != nil {
		t.Fatalf("read pkg/client/client.go: %v", err)
	}
	killRunDoc := regexp.MustCompile(`(?s)// KillRun initiates[^\n]*(\n//[^\n]*)*`).FindString(string(client))
	if killRunDoc == "" {
		t.Fatal("pkg/client/client.go: could not find KillRun's doc comment — update this guard's anchor if it moved")
	}
	if regexp.MustCompile(`(?i)^// KillRun initiates the kill sequence for a run: sandbox teardown,`).MatchString(killRunDoc) {
		t.Errorf("pkg/client/client.go's KillRun doc comment still leads with sandbox teardown (pre-C002 order): %q", killRunDoc)
	}
	if !regexp.MustCompile(`(?i)state transition|compare-and-swap|CAS|KILLED state`).MatchString(killRunDoc) {
		t.Errorf("pkg/client/client.go's KillRun doc comment doesn't mention the CAS-first order: %q", killRunDoc)
	}
}
