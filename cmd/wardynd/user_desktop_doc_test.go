// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUserDesktopDocNamesTheHybridEnrolmentPath(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "ENV.md"))
	if err != nil {
		t.Fatal(err)
	}
	const marker = "| `WARDYN_USER_DESKTOP` |"
	_, row, found := strings.Cut(string(doc), marker)
	if !found {
		t.Fatal("ENV.md has no WARDYN_USER_DESKTOP row")
	}
	row, _, _ = strings.Cut(row, "\n")
	for _, want := range []string{"WARDYN_ORG_URL", "DESKTOP.md#enrolling-into-an-org-control-plane"} {
		if !strings.Contains(row, want) {
			t.Errorf("WARDYN_USER_DESKTOP row does not name %s", want)
		}
	}
	desktop, err := os.ReadFile(filepath.Join(root, "docs", "DESKTOP.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(desktop), "### Enrolling into an org control plane") {
		t.Error("DESKTOP.md hybrid enrolment link target is missing")
	}
}
