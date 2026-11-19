// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
)

// folderSharesStage: OK only with a root and an allowed Full Disk Access reading that a tools update has
// not made doubtful; otherwise a skip that names the one thing to set. Only a guest agent signed ad hoc
// loses the grant at a tools update; one signed on this Mac keeps the reading, as the base last read it.
func TestFolderSharesStage(t *testing.T) {
	read := time.Date(2026, 11, 1, 9, 0, 0, 0, time.Local)
	later := read.Add(48 * time.Hour)
	roots := []string{"/Users/a/Work", "/Volumes/Data"}
	const thisMac, adHoc, none = "this_mac", "ad_hoc", ""
	for _, tc := range []struct {
		name         string
		roots        []string
		fda, sig     string
		readAt, tool time.Time
		status       doctor.Status
		want         []string
	}{
		{"no root", nil, "allowed", thisMac, read, read, doctor.StatusSkip, []string{"no drive root", "setup --drive-root <dir>"}},
		{"refused", roots, "refused", thisMac, read, read, doctor.StatusSkip, []string{"not granted", "within 10 seconds", "edit-base"}},
		{"not read", roots, "", thisMac, time.Time{}, later, doctor.StatusSkip, []string{"has not been read", "edit-base"}},
		{"no record", roots, "", none, time.Time{}, time.Time{}, doctor.StatusSkip, []string{"has not been read"}},
		{"tools newer, ad hoc", roots, "allowed", adHoc, read, later, doctor.StatusSkip, []string{"tools were updated after Full Disk Access was last read", "2026-11-01", "2026-11-03", "signed ad hoc loses it"}},
		{"tools newer, signed on this Mac", roots, "allowed", thisMac, read, later, doctor.StatusOK, []string{"/Users/a/Work, /Volumes/Data (from runnerd.json", "allowed (as the base last read it, 2026-11-01)", "workspace roots are set on its server"}},
		{"tools newer, no signature recorded", roots, "allowed", none, read, later, doctor.StatusOK, []string{"allowed (as the base last read it, 2026-11-01)"}},
		{"tools not newer, ad hoc", roots, "allowed", adHoc, read, read, doctor.StatusOK, []string{"allowed"}},
		{"read date unknown, ad hoc", roots, "allowed", adHoc, time.Time{}, later, doctor.StatusOK, []string{"allowed (as the base last read it)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := folderSharesStage(tc.roots, "runnerd.json", tc.fda, tc.sig == adHoc, tc.readAt, tc.tool)
			if st.Name != "folder-shares" || st.Status != tc.status {
				t.Fatalf("stage %s is %s, want %s: %s", st.Name, st.Status, tc.status, st.Detail)
			}
			for _, w := range tc.want {
				if !strings.Contains(st.Detail, w) {
					t.Errorf("detail lacks %q: %s", w, st.Detail)
				}
			}
		})
	}
}
