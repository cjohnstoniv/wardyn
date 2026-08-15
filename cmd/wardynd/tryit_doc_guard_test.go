// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestTRYITDoc_NoStaleReplayTab is the W21-S1-8 regression: docs/TRY-IT.md used
// to send the first-run user to a "Replay tab" that has never existed — the run
// detail screen's tab is named "Recording" (run-detail.tsx's Tab type union).
// Live viewing is `wardyn attach <id>` / the run's attach terminal, not a
// replay surface (the recording only shows the finished capture after the
// session ends). Anchor both halves so a rename on either side breaks this
// loudly instead of the doc silently drifting again.
func TestTRYITDoc_NoStaleReplayTab(t *testing.T) {
	root := repoRoot(t)

	doc, err := os.ReadFile(filepath.Join(root, "docs", "TRY-IT.md"))
	if err != nil {
		t.Fatalf("read docs/TRY-IT.md: %v", err)
	}
	if regexp.MustCompile(`(?i)replay tab|→ Replay\b`).Match(doc) {
		t.Error(`docs/TRY-IT.md still points at a "Replay tab" — the real tab is "Recording" (see run-detail.tsx's Tab type)`)
	}

	tabsFile := filepath.Join(root, "ui", "src", "app", "components", "screens", "run-detail.tsx")
	tabs, err := os.ReadFile(tabsFile)
	if err != nil {
		t.Fatalf("read run-detail.tsx: %v", err)
	}
	tabType := regexp.MustCompile(`type Tab = [^\n]+`).FindString(string(tabs))
	if tabType == "" {
		t.Fatal("run-detail.tsx: could not find the Tab type union — update this guard's anchor if it was renamed")
	}
	if !regexp.MustCompile(`"recording"`).MatchString(tabType) {
		t.Errorf("run-detail.tsx's Tab type no longer has \"recording\": %s", tabType)
	}
	if regexp.MustCompile(`"replay"`).MatchString(tabType) {
		t.Errorf("run-detail.tsx's Tab type now has \"replay\" — docs/TRY-IT.md's fix assumed it never would: %s", tabType)
	}
}
