// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/basesettings"
)

// BaseSettingsWarnings turns tools.json's base_settings_skipped into doctor WARN lines: one per reason, in
// the order the reasons first appear, with the rows named and a long list cut short.
func TestBaseSettingsWarnings(t *testing.T) {
	if got := BaseSettingsWarnings(nil); got != nil {
		t.Fatalf("none skipped: %q", got)
	}
	const quiet = "nobody at the console"
	skipped := []basesettings.Skipped{
		{Name: "SetupAssistant.DidSeeCloudSetup", Reason: quiet},
		{Name: "spotlight-off", Reason: "exit 1: boom"},
		{Name: "SetupAssistant.DidSeeSiriSetup", Reason: quiet},
	}
	want := []string{
		"base settings skipped, nobody at the console: SetupAssistant.DidSeeCloudSetup, SetupAssistant.DidSeeSiriSetup",
		"base settings skipped, exit 1: boom: spotlight-off",
	}
	if got := BaseSettingsWarnings(skipped); !slices.Equal(got, want) {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
	var many []basesettings.Skipped
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		many = append(many, basesettings.Skipped{Name: n, Reason: quiet})
	}
	if got, want := BaseSettingsWarnings(many), []string{"base settings skipped, nobody at the console: a, b, c, d, e, f and 2 more"}; !slices.Equal(got, want) {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
	// The old-agent line says what to do.
	old := []basesettings.Skipped{{Name: "base-settings", Reason: "agent predates this setting; run `wardyn-runnerd macos-vm update-tools` again"}}
	if got, want := BaseSettingsWarnings(old), []string{"base settings skipped, agent predates this setting; run `wardyn-runnerd macos-vm update-tools` again: base-settings"}; !slices.Equal(got, want) {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

// The guest-agent stage carries the base settings a tools update skipped as warnings, and they never fail it.
func TestGuestAgentStageWarnsOnSkippedBaseSettings(t *testing.T) {
	rec := macvm.ToolsRecord{AgentSignature: macvm.AgentSignedThisMac,
		BaseSettingsSkipped: []basesettings.Skipped{{Name: "mdutil", Reason: "exit status 1"}}}
	w := guestAgentWarnings(rec)
	if len(w) == 0 {
		t.Fatal("no warning for a skipped base setting")
	}
	if st := guestAgentStage(rec, w); st.Status == doctor.StatusFail || !strings.Contains(st.Detail, "mdutil") {
		t.Fatalf("stage: %+v", st)
	}
}
