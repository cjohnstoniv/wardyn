// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestDriveRefusalSentencesMatchGolden pins the drives[] sentences to the table
// the console's twin (ui/src/app/lib/drive-refusals.ts) is pinned to.
func TestDriveRefusalSentencesMatchGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "ui", "src", "app", "lib", "drive-refusals.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden []struct {
		Key  string   `json:"key"`
		Args []string `json:"args"`
		Text string   `json:"text"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	atoi := func(s string) int {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("golden arg %q is not a number", s)
		}
		return n
	}
	got := map[string]func(a []string) string{
		"SELECTION_CONFLICT":   func([]string) string { return driveSelectionConflictMsg() },
		"INVALID":              func(a []string) string { return driveInvalidMsg(atoi(a[0])) },
		"TOO_MANY":             func(a []string) string { return driveTooManyMsg(atoi(a[0])) },
		"NOT_FOUND":            func(a []string) string { return driveNotFoundMsg(a[0]) },
		"NOT_IN_POOL":          func(a []string) string { return driveNotInPoolMsg(a[0]) },
		"TARGET_CONFLICT":      func(a []string) string { return driveTargetConflictMsg(a[0], a[1], a[2]) },
		"SHARE_BESIDE_MANAGED": func(a []string) string { return driveShareBesideManagedMsg(a[0], a[1]) },

		"MEMBER_REQUIRES_PERSON":           func([]string) string { return memberDriveRequiresPersonMsg() },
		"MEMBER_DISABLED":                  func([]string) string { return memberDrivesDisabledMsg() },
		"MEMBER_DENIED_BY_PROFILE":         func(a []string) string { return memberDrivesDeniedByProfileMsg(a[0]) },
		"MEMBER_LIMIT_REACHED":             func(a []string) string { return memberDriveLimitReachedMsg(atoi(a[0]), atoi(a[1])) },
		"MEMBER_UNSUPPORTED":               func([]string) string { return memberDriveUnsupportedMsg() },
		"MEMBER_UNSIZED":                   func([]string) string { return memberDriveUnsizedMsg() },
		"MEMBER_DELETE_UNAVAILABLE_CREATE": func([]string) string { return memberDriveDeleteUnavailableCreateMsg() },
		"MEMBER_DELETE_UNAVAILABLE":        func([]string) string { return memberDriveDeleteUnavailableMsg() },
		"MEMBER_NAME_INVALID":              func([]string) string { return memberDriveNameInvalidMsg() },
		"MEMBER_NAME_TAKEN":                func(a []string) string { return memberDriveNameTakenMsg(a[0]) },
		"MEMBER_IN_USE":                    func([]string) string { return memberDriveInUseMsg() },
		"MEMBER_BUSY":                      func([]string) string { return memberDriveBusyMsg() },
		"MEMBER_DELETE_FAILED":             func([]string) string { return memberDriveDeleteFailedMsg() },
		"MEMBER_RATE_LIMITED":              func([]string) string { return memberDriveRateLimitedMsg() },
	}
	if len(golden) != len(got) {
		t.Fatalf("golden has %d sentences, the server has %d", len(golden), len(got))
	}
	for _, g := range golden {
		f, ok := got[g.Key]
		if !ok {
			t.Fatalf("golden names %s, which the server has no sentence for", g.Key)
		}
		if s := f(g.Args); s != g.Text {
			t.Errorf("%s = %q, golden %q", g.Key, s, g.Text)
		}
	}
}
