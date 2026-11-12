// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A pass back's record puts its outcome first and cuts only the reason: a 2000-rune runner reason cannot push
// how the sandbox ended (kept for a later sweep, or stopped with the count left behind) out of the record.
func TestFolderBackText_OutcomeSurvivesALongReason(t *testing.T) {
	long := strings.Repeat("é", 2000)
	bounded := strings.Repeat("é", maxFolderReason) + "..."

	_, kept := folderBackResume(nil, &runnerwire.FolderPass{}, long)
	const keptHead = "the sandbox is kept with its agent stopped and no egress, and a later sweep, about hourly, tries the sync again: "
	if kept != keptHead+bounded {
		t.Errorf("kept text = %d runes, want the outcome, then the reason cut to %d runes", len([]rune(kept)), maxFolderReason)
	}

	stopped := folderBackGivenUp(long, "2 passes in a row brought nothing back", 3)
	const stoppedHead = "the sandbox was stopped with changed files not brought back: 3, because 2 passes in a row brought nothing back: "
	if stopped != stoppedHead+bounded {
		t.Errorf("stopped text = %d runes, want the outcome, then the reason cut to %d runes", len([]rune(stopped)), maxFolderReason)
	}

	notPreserved := folderNotPreservedText(1, long, strings.Repeat("b", 2000))
	if n := len([]rune(notPreserved)); n > 120+2*(maxFolderReason+3)+4 || !strings.Contains(notPreserved, ": "+bounded) || !strings.HasSuffix(notPreserved, "; "+strings.Repeat("b", maxFolderReason)+"...") {
		t.Errorf("not-preserved text is %d runes, want each reason cut to %d runes", n, maxFolderReason)
	}

	for name, text := range map[string]string{"kept": kept, "stopped": stopped} {
		if got := folderPassRecord(time.Now(), runnerwire.FolderPass{}, text, true).Error; got != text {
			t.Errorf("%s: the record cut the text to %d runes; want all %d, outcome included", name, len([]rune(got)), len([]rune(text)))
		}
	}
}

// The count of what a stopped sandbox leaves behind comes first, before the cause.
func TestFolderBackText_GivenUpCountFirst(t *testing.T) {
	for _, tc := range []struct {
		unsynced int
		want     string
	}{
		{0, "the sandbox was stopped with its changes not brought back yet, uncounted, because why: reason"},
		{1, "the sandbox was stopped with changed files not brought back: 1, because why: reason"},
	} {
		if got := folderBackGivenUp("reason", "why", tc.unsynced); got != tc.want {
			t.Errorf("unsynced %d: got %q, want %q", tc.unsynced, got, tc.want)
		}
	}
}

// boundReason cuts at maxFolderReason runes and marks the cut: 0 runes is the empty-message line, 300 is kept
// whole, 301 and 2000 are cut to 300 and the marker.
func TestBoundReason_Boundaries(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "the sync reported a failure without a message"},
		{maxFolderReason, strings.Repeat("x", maxFolderReason)},
		{maxFolderReason + 1, strings.Repeat("x", maxFolderReason) + "..."},
		{2000, strings.Repeat("x", maxFolderReason) + "..."},
	} {
		if got := boundReason(strings.Repeat("x", tc.n)); got != tc.want {
			t.Errorf("%d runes: got %d runes %q, want %q", tc.n, len([]rune(got)), got, tc.want)
		}
	}
}
