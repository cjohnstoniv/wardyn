// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"os"
	"regexp"
	"testing"
)

// TestApprovalKindsCoversEveryConstant is what makes ApprovalKinds a DERIVED
// closed set rather than a second hand-typed list. internal/db's
// closedEnumChecks compares the approvals.kind CHECK against this slice, so a
// fifth kind declared in Go and left out of it would be written by the control
// plane and rejected by Postgres with every gate green — the blindness 0062
// closed for approvals.state and 0064 closes for approvals.kind.
//
// It reads the SOURCE rather than reflecting over values, for
// TestApprovalStatesCoversEveryConstant's reason: a constant nobody references
// is invisible to reflection but plain in the file.
func TestApprovalKindsCoversEveryConstant(t *testing.T) {
	src, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatalf("read types.go: %v", err)
	}
	decl := regexp.MustCompile(`(?m)^\s*Approval\w+\s+ApprovalKind\s*=\s*"([^"]+)"`)
	found := decl.FindAllStringSubmatch(string(src), -1)
	if len(found) == 0 {
		t.Fatal("no ApprovalKind constant declarations found in types.go — re-anchor this guard")
	}
	listed := map[ApprovalKind]bool{}
	for _, k := range ApprovalKinds {
		if listed[k] {
			t.Errorf("ApprovalKinds lists %q twice", k)
		}
		listed[k] = true
	}
	declared := map[ApprovalKind]bool{}
	for _, m := range found {
		k := ApprovalKind(m[1])
		declared[k] = true
		if !listed[k] {
			t.Errorf("ApprovalKind %q is declared but missing from types.ApprovalKinds — "+
				"the migrations' approvals.kind CHECK is compared against that slice, so a kind "+
				"left out of it can be written by Go and rejected by Postgres with every gate green", k)
		}
	}
	for k := range listed {
		if !declared[k] {
			t.Errorf("types.ApprovalKinds contains %q, which is not a declared ApprovalKind constant", k)
		}
	}
}
