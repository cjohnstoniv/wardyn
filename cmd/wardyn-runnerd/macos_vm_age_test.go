// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// The caps carry when the base was prepared and its build, from its manifest; a manifest with neither
// (an older base) sends neither.
func TestMacOSVMBaseCarriesItsAge(t *testing.T) {
	m := newMacLedgerRig(t).open(t)
	if _, b := m.base(); b.CreatedAt != "" || b.BuildVersion != "" {
		t.Fatalf("a manifest with no age sent %+v", b)
	}
	man := `{"format":1,"state":"ready","guest_os":"macOS 26.6.2 (25G83)","build_version":"25G83","created_at":"2026-10-28T09:30:00+02:00","agent_version":"1.0.0","min_cpus":4,"min_memory_mib":8192,"desktop":"control","harnesses":["claude-code"]}`
	if err := os.WriteFile(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile), []byte(man), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, b := m.base(); b.CreatedAt != "2026-10-28T07:30:00Z" || b.BuildVersion != "25G83" {
		t.Fatalf("base = %+v, want created_at in UTC and the build", b)
	}
}

// The status base line names when the base was prepared and how long ago, and this Mac's macOS beside the
// base's; a base that is not ready, or an older runner's with no date, has neither.
func TestMacOSVMStatusBaseLine(t *testing.T) {
	prev := statusNow
	statusNow = func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { statusNow = prev })
	prepared := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	line := func(st macOSVMStatus) string {
		var out strings.Builder
		st.writeText(&out)
		return strings.SplitN(out.String(), "\n", 2)[0]
	}
	want := "base: ready, macOS 26.6.2 (25G83), prepared " + prepared.Local().Format(time.DateOnly) + " (2 days ago); this Mac: macOS 26.7"
	if got := line(macOSVMStatus{State: runnerwire.MacVMBaseReady, Base: "macOS 26.6.2 (25G83)", PreparedAt: prepared.Format(time.RFC3339), HostOS: "26.7"}); got != want {
		t.Fatalf("base line = %q, want %q", got, want)
	}
	if got := line(macOSVMStatus{State: runnerwire.MacVMBaseReady, Base: "macOS 26.6.2 (25G83)"}); got != "base: ready, macOS 26.6.2 (25G83)" {
		t.Fatalf("a base with no date = %q", got)
	}
	if got := line(macOSVMStatus{State: runnerwire.MacVMBasePreparing, Base: "macOS 26.7", PreparedAt: prepared.Format(time.RFC3339), HostOS: "26.7"}); strings.Contains(got, "prepared") || strings.Contains(got, "this Mac") {
		t.Fatalf("a base still preparing = %q", got)
	}
}

// One day reads "1 day ago", every other count "<n> days ago", on the status base line too.
func TestMacOSVMStatusBaseLineDayPlural(t *testing.T) {
	prev := statusNow
	statusNow = func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { statusNow = prev })
	for _, tc := range []struct {
		prepared time.Time
		want     string
	}{
		{time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC), "(1 day ago)"},
		{time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC), "(2 days ago)"},
		{time.Date(2026, 10, 30, 9, 30, 0, 0, time.UTC), "(0 days ago)"},
	} {
		var out strings.Builder
		macOSVMStatus{State: runnerwire.MacVMBaseReady, Base: "macOS 26.6.2", PreparedAt: tc.prepared.Format(time.RFC3339)}.writeText(&out)
		if line := strings.SplitN(out.String(), "\n", 2)[0]; !strings.Contains(line, tc.want) {
			t.Errorf("base line %q, want it to contain %q", line, tc.want)
		}
	}
}
