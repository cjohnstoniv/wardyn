// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package basesettings

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

var rel = Release{Version: "26.6.2", Build: "25G83"}

func rendered(t *testing.T) map[string]Row {
	t.Helper()
	rows, err := Rows(rel)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Row{}
	for _, r := range rows {
		if _, dup := out[r.Name]; dup {
			t.Fatalf("two rows are named %s", r.Name)
		}
		out[r.Name] = r
	}
	return out
}

// The value types are part of what macOS reads: an int where a bool belongs is ignored. Each is pinned
// against the argv word for word.
func TestRowValueTypesArePinned(t *testing.T) {
	const sa = "/Users/agent/Library/Preferences/com.apple.SetupAssistant"
	rows := rendered(t)
	want := map[string][]string{
		"SetupAssistant.MiniBuddyLaunchReason":              {"/usr/bin/defaults", "write", sa, "MiniBuddyLaunchReason", "-int", "0"},
		"SetupAssistant.MiniBuddyShouldLaunchToResumeSetup": {"/usr/bin/defaults", "write", sa, "MiniBuddyShouldLaunchToResumeSetup", "-bool", "false"},
		"SetupAssistant.LastSeenCloudProductVersion":        {"/usr/bin/defaults", "write", sa, "LastSeenCloudProductVersion", "-string", "26.6.2"},
		"SetupAssistant.LastSeenBuddyBuildVersion":          {"/usr/bin/defaults", "write", sa, "LastSeenBuddyBuildVersion", "-string", "25G83"},
		"SetupAssistant.LastPreLoginTasksPerformedBuild":    {"/usr/bin/defaults", "write", sa, "LastPreLoginTasksPerformedBuild", "-string", "25G83"},
		"SetupAssistant.LastPreLoginTasksPerformedVersion":  {"/usr/bin/defaults", "write", sa, "LastPreLoginTasksPerformedVersion", "-string", "26.6.2"},
		"SetupAssistant.DidSeeCloudSetup":                   {"/usr/bin/defaults", "write", sa, "DidSeeCloudSetup", "-bool", "true"},
		"HIToolbox.AppleFnUsageType":                        {"/usr/bin/defaults", "write", "/Users/agent/Library/Preferences/com.apple.HIToolbox", "AppleFnUsageType", "-int", "0"},
		"Global.NSQuitAlwaysKeepsWindows":                   {"/usr/bin/defaults", "write", "-g", "NSQuitAlwaysKeepsWindows", "-bool", "false"},
		"loginwindow.TALLogoutSavesState":                   {"/usr/bin/defaults", "-currentHost", "write", "com.apple.loginwindow", "TALLogoutSavesState", "-bool", "false"},
		"spotlight-off":                                     {"/usr/bin/mdutil", "-a", "-i", "off"},
		"CrashReporter.AutoSubmit":                          {"/usr/bin/defaults", "write", "/Library/Application Support/CrashReporter/DiagnosticMessagesHistory", "AutoSubmit", "-bool", "false"},
		"CrashReporter.ThirdPartyDataSubmit":                {"/usr/bin/defaults", "write", "/Library/Application Support/CrashReporter/DiagnosticMessagesHistory", "ThirdPartyDataSubmit", "-bool", "false"},
		"CrashReporter.mode":                                {"/bin/chmod", "644", "/Library/Application Support/CrashReporter/DiagnosticMessagesHistory.plist"},
		"CrashReporter.group":                               {"/usr/bin/chgrp", "admin", "/Library/Application Support/CrashReporter/DiagnosticMessagesHistory.plist"},
	}
	for name, argv := range want {
		r, ok := rows[name]
		if !ok {
			t.Errorf("no row %s", name)
			continue
		}
		if !slices.Equal(r.Argv, argv) {
			t.Errorf("%s argv\n got %q\nwant %q", name, r.Argv, argv)
		}
	}
	// Every pane the plan lists is marked as seen with a bool true, and nothing else is a DidSee key.
	panes := []string{"Accessibility", "ActivationLock", "AppStore", "AppearanceSetup", "ApplePaySetup", "CloudSetup", "LockdownMode", "Privacy",
		"ScreenTime", "SetupSequence", "SiriSetup", "SyncSetup", "SyncSetup2", "TermsOfAddress", "TouchIDSetup", "iCloudLoginForStorageServices"}
	for _, p := range panes {
		r, ok := rows["SetupAssistant.DidSee"+p]
		if !ok || !slices.Equal(r.Argv[3:], []string{"DidSee" + p, "-bool", "true"}) {
			t.Errorf("DidSee%s: %+v", p, r)
		}
	}
	if ai, ok := rows["SetupAssistant.DidSeeAppleIntelligence"]; !ok || !ai.Optional {
		t.Errorf("DidSeeAppleIntelligence must be present and Optional: %+v", ai)
	}
}

// Users: the diagnostics files and Spotlight are root's; every Setup Assistant, HIToolbox, global and
// loginwindow row is the agent's. The keys read from a real macOS 26 guest are not Optional; the
// ones with no source are.
func TestRowUsersAndOptional(t *testing.T) {
	rows := rendered(t)
	for name, r := range rows {
		switch {
		case strings.HasPrefix(name, "CrashReporter.") || name == "spotlight-off":
			if r.User != Root {
				t.Errorf("%s runs as %q, want root", name, r.User)
			}
		default:
			if r.User != Agent {
				t.Errorf("%s runs as %q, want agent", name, r.User)
			}
		}
		if r.Step == "" || r.Name == "" || len(r.Argv) == 0 {
			t.Errorf("%+v is incomplete", r)
		}
	}
	for _, n := range []string{"SetupAssistant.DidSeeCloudSetup", "SetupAssistant.DidSeeSiriSetup", "SetupAssistant.LastSeenCloudProductVersion"} {
		if rows[n].Optional {
			t.Errorf("%s was read from a real guest and is not Optional", n)
		}
	}
	for _, n := range []string{"SetupAssistant.DidSeeAppleIntelligence", "SetupAssistant.SkipFirstLoginOptimization", "loginwindow.TALLogoutSavesState"} {
		if !rows[n].Optional {
			t.Errorf("%s has no source and must be Optional", n)
		}
	}
}

// Every path is absolute, there is no tilde, and no placeholder is left in a rendered argv.
func TestRowsAreRenderedAndAbsolute(t *testing.T) {
	for name, r := range rendered(t) {
		for _, w := range r.Argv {
			if strings.Contains(w, "{{") || strings.Contains(w, "~") {
				t.Errorf("%s: %q is unrendered or a tilde path", name, w)
			}
		}
		if r.Check != nil && (!strings.HasPrefix(r.Check.Path, "/") || r.Check.Key == "" || len(r.Check.Want) == 0) {
			t.Errorf("%s: read-back %+v", name, r.Check)
		}
	}
	for _, p := range Removals {
		if !strings.HasPrefix(p.Path, "/Users/agent/") {
			t.Errorf("removal %s: %q is not an absolute path in the agent's home", p.Name, p.Path)
		}
	}
}

// The host renders the stamps from sw_vers, and a value outside the fixed patterns is refused before any row
// is made.
func TestRowsRefuseAReleaseOutsideThePatterns(t *testing.T) {
	for _, tc := range []struct{ version, build string }{
		{"26.6.2; rm -rf /", "25G83"},
		{"26", "25G83"},
		{"26.6.2.1", "25G83"},
		{"", "25G83"},
		{"26.6.2", "25G83 extra"},
		{"26.6.2", "25g83"},
		{"26.6.2", "G8325"},
		{"26.6.2", ""},
		{"26.6.2", "25G83\n25G84"},
	} {
		if _, err := Rows(Release{Version: tc.version, Build: tc.build}); !errors.Is(err, ErrRelease) {
			t.Errorf("%+v: err %v, want ErrRelease", tc, err)
		}
	}
	for _, ok := range []Release{{"26.6.2", "25G83"}, {"26.0", "25A5316i"}, {"27.1", "26B91"}} {
		if _, err := Rows(ok); err != nil {
			t.Errorf("%+v refused: %v", ok, err)
		}
	}
	// sw_vers prints a trailing newline; ParseRelease takes it off and checks the rest.
	if r, err := ParseRelease("26.6.2\n", "25G83\n"); err != nil || r != rel {
		t.Errorf("ParseRelease = %+v, %v", r, err)
	}
	if _, err := ParseRelease("26.6.2\nx", "25G83"); !errors.Is(err, ErrRelease) {
		t.Errorf("ParseRelease with a second line: %v", err)
	}
}

// Terminal's kill and saved-state removal are the only entries the edit-base finish runs.
func TestTerminalEntriesAreMarked(t *testing.T) {
	var kills, removals []string
	for _, k := range Kills {
		if k.Terminal {
			kills = append(kills, k.Process)
		}
	}
	for _, r := range Removals {
		if r.Terminal {
			removals = append(removals, r.Path)
		}
	}
	if !slices.Equal(kills, []string{"Terminal"}) {
		t.Errorf("terminal kills %q", kills)
	}
	if !slices.Equal(removals, []string{"/Users/agent/Library/Saved Application State/com.apple.Terminal.savedState"}) {
		t.Errorf("terminal removals %q", removals)
	}
	if !slices.ContainsFunc(Kills, func(k Kill) bool { return k.Process == "Setup Assistant" && !k.Terminal }) {
		t.Error("Setup Assistant is not killed before the rows are written")
	}
}

// Safari Safe Browsing is Optional, the agent's, and in the stage-2 table only.
func TestSafariSafeBrowsingIsStage2Only(t *testing.T) {
	const prefs = "/Users/agent/Library/Containers/com.apple.Safari/Data/Library/Preferences/com.apple.Safari"
	r, ok := rendered(t)["safari-safe-browsing"]
	if !ok || !r.Optional || r.User != Agent || !r.Stage2Only {
		t.Fatalf("row %+v", r)
	}
	if want := []string{"/usr/bin/defaults", "write", prefs, "WarnAboutFraudulentWebsites", "-bool", "false"}; !slices.Equal(r.Argv, want) {
		t.Errorf("argv %q", r.Argv)
	}
	if r.Read == nil || !slices.Equal(r.Read.Argv, []string{"/usr/bin/defaults", "read", prefs, "WarnAboutFraudulentWebsites"}) || r.Read.Want != "0" {
		t.Errorf("read-back %+v", r.Read)
	}
	rows, err := Stage1Rows(rel)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(rows, func(r Row) bool { return r.Name == "safari-safe-browsing" || r.Stage2Only }) {
		t.Error("stage 1 would write a stage-2-only row")
	}
}
