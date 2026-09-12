// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"os"
	"regexp"
	"testing"
)

// TestApprovalStatesCoversEveryConstant is what makes ApprovalStates a DERIVED
// set rather than a second hand-typed list. Go cannot enumerate the constants of
// a named string type at runtime, so the enumeration is read out of the
// declaration itself: every `ApprovalX ApprovalState = "VALUE"` in types.go must
// appear in ApprovalStates, and nothing else may.
//
// Without it, the migration parity guard (internal/db's closedEnumChecks) is
// exactly as blind as the literal it replaced — a sixth state would be defined
// in Go, rejected by the database's CHECK, and reported clean by every gate.
// That is the COMPLETED-state incident, which is why the CHANGELOG's "the Go set
// and the database are compared from now on" has to be true and not merely
// arranged to look true.
func TestApprovalStatesCoversEveryConstant(t *testing.T) {
	src, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatalf("read types.go: %v", err)
	}
	decl := regexp.MustCompile(`(?m)^\s*Approval\w+\s+ApprovalState\s*=\s*"([^"]+)"`)
	found := decl.FindAllStringSubmatch(string(src), -1)
	if len(found) == 0 {
		t.Fatal("no ApprovalState constant declarations found in types.go — re-anchor this guard")
	}
	listed := map[ApprovalState]bool{}
	for _, s := range ApprovalStates {
		if listed[s] {
			t.Errorf("ApprovalStates lists %q twice", s)
		}
		listed[s] = true
	}
	declared := map[ApprovalState]bool{}
	for _, m := range found {
		st := ApprovalState(m[1])
		declared[st] = true
		if !listed[st] {
			t.Errorf("ApprovalState %q is declared but missing from types.ApprovalStates — "+
				"the migrations' approvals.state CHECK is compared against that slice, so a state "+
				"left out of it can be written by Go and rejected by Postgres with every gate green", st)
		}
	}
	for st := range listed {
		if !declared[st] {
			t.Errorf("types.ApprovalStates contains %q, which is not a declared ApprovalState constant", st)
		}
	}
}
