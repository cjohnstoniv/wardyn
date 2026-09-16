// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// sdk_doc_test.go is the derived-set guard docs/sdk.md's ApprovalState line
// needed and didn't have: X1a-F7 found the doc's "also Approved / Denied /
// Expired" comment silently missing Cancelled (added in 0.7.2, types.go) —
// the doc's list is hand-typed prose, so nothing caught the drift. Mirrors
// internal/types/approval_states_test.go's derived-set pattern (reading the
// enumeration back out of the declaration) instead of a second hand-typed
// list here too.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func repoRootForSDKDoc(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}

// TestSDKDoc_ApprovalStateListCoversEveryState pins docs/sdk.md's "// also ...
// (ApprovalState)" comment line against types.ApprovalStates — the same
// derived set the migration parity guard (internal/db's closedEnumChecks) and
// TestApprovalStatesCoversEveryConstant already trust — so a state added to
// the enum without updating the doc's hand-written list fails here instead of
// silently going stale a second time.
func TestSDKDoc_ApprovalStateListCoversEveryState(t *testing.T) {
	root := repoRootForSDKDoc(t)
	path := filepath.Join(root, "docs", "sdk.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read docs/sdk.md: %v", err)
	}
	line := regexp.MustCompile(`(?m)^_ = client\.ApprovalPending // also (.+) \(ApprovalState\)$`).FindStringSubmatch(string(b))
	if line == nil {
		t.Fatal("docs/sdk.md no longer has the '_ = client.ApprovalPending // also ... (ApprovalState)' line — re-anchor this guard")
	}
	documented := map[string]bool{"Pending": true} // the constant itself, not repeated after "also"
	for _, w := range strings.Split(line[1], "/") {
		documented[strings.TrimSpace(w)] = true
	}
	for _, st := range types.ApprovalStates {
		// ApprovalState constants are declared PascalCase after "Approval"
		// (ApprovalCancelled -> "Cancelled"); the doc's comment uses the same
		// short form, not the wire value ("CANCELLED").
		short := approvalStateShortName(st)
		if !documented[short] {
			t.Errorf("types.ApprovalStates has wire value %q but docs/sdk.md's ApprovalState comment line does not list %q", st, short)
		}
	}
}

// approvalStateShortName converts an ApprovalState wire value ("CANCELLED")
// to the PascalCase short form docs/sdk.md's comment uses ("Cancelled").
func approvalStateShortName(st types.ApprovalState) string {
	s := strings.ToLower(string(st))
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
