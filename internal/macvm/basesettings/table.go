// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package basesettings

import "strconv"

const (
	prefs        = "/Users/agent/Library/Preferences/"
	saDomain     = prefs + "com.apple.SetupAssistant"
	diagDomain   = "/Library/Application Support/CrashReporter/DiagnosticMessagesHistory"
	defaultsTool = "/usr/bin/defaults"
	safariPrefs  = "/Users/agent/Library/Containers/com.apple.Safari/Data/Library/Preferences/com.apple.Safari"
)

// The value types are part of what macOS reads, and tests pin them: MiniBuddyLaunchReason and
// AppleFnUsageType are ints, the stamps strings, the DidSee keys and the rest bools.
type valueKind int

const (
	kInt valueKind = iota
	kBool
	kString
)

// def is a `defaults write` of one key of the property list at domain (an absolute path without .plist),
// with the read-back of the file it lands in. val is a bool as "true" or "false", an int as digits, a
// string as given (it may hold a release token).
func def(user User, name, domain, key string, kind valueKind, val string, optional bool) Row {
	flag := [...]string{"-int", "-bool", "-string"}[kind]
	want := []string{val}
	switch {
	case kind == kBool && val == "true":
		want = []string{"1", "true"}
	case kind == kBool:
		want = []string{"0", "false"}
	case kind == kString:
		want = []string{strconv.Quote(val)}
	}
	return Row{Name: name, User: user, Argv: []string{defaultsTool, "write", domain, key, flag, val}, Optional: optional, Step: Step,
		Check: &Check{Path: domain + ".plist", Key: key, Want: want}}
}

// sa is one key of the agent's Setup Assistant domain.
func sa(key string, kind valueKind, val string, optional bool) Row {
	return def(Agent, "SetupAssistant."+key, saDomain, key, kind, val, optional)
}

// didSee marks one pane as seen. The panes named by a real macOS 26 guest's own `defaults read` are not
// Optional; the others are, since no documentation names them.
func didSee(pane string, optional bool) Row { return sa("DidSee"+pane, kBool, "true", optional) }

// table is the rows with their release tokens in place. Sources: the keys a real macOS 26.6.2 guest listed
// (DidSee* and LastSeenCloudProductVersion for Setup Assistant), Apple's documentation of
// NSQuitAlwaysKeepsWindows and of mdutil, and the DiagnosticMessagesHistory keys; every other key is
// Optional and unverified on macOS 26.
var table = []Row{
	// Setup Assistant's resume and first-login work.
	sa("MiniBuddyLaunchReason", kInt, "0", true),
	sa("MiniBuddyShouldLaunchToResumeSetup", kBool, "false", true),
	sa("SkipFirstLoginOptimization", kBool, "true", true),
	sa("SkipExpressSettingsUpdating", kBool, "true", true),
	// The stamps that say the panes were seen on this macOS. A different macOS makes Setup Assistant ask again.
	sa("LastPreLoginTasksPerformedBuild", kString, tokBuild, true),
	sa("LastPreLoginTasksPerformedVersion", kString, tokVersion, true),
	sa("LastSeenBuddyBuildVersion", kString, tokBuild, true),
	sa("LastSeenCloudProductVersion", kString, tokVersion, false),
	sa("LastSeenDiagnosticsProductVersion", kString, tokVersion, true),
	sa("LastSeenAgeRangeSelectionProductVersion", kString, tokVersion, true),
	// The panes.
	didSee("Accessibility", true),
	didSee("ActivationLock", false),
	didSee("AppStore", false),
	didSee("AppearanceSetup", true),
	didSee("ApplePaySetup", false),
	didSee("CloudSetup", false),
	didSee("LockdownMode", false),
	didSee("Privacy", true),
	didSee("ScreenTime", true),
	didSee("SetupSequence", true),
	didSee("SiriSetup", false),
	didSee("SyncSetup", false),
	didSee("SyncSetup2", false),
	didSee("TermsOfAddress", false),
	didSee("TouchIDSetup", false),
	didSee("iCloudLoginForStorageServices", true),
	didSee("AppleIntelligence", true),

	// Dictation: the Fn (globe) key does nothing, so a first key press raises no microphone prompt.
	def(Agent, "HIToolbox.AppleFnUsageType", prefs+"com.apple.HIToolbox", "AppleFnUsageType", kInt, "0", true),

	// Terminal: no windows are kept across a quit, here and at the login window. The global preference is
	// documented; loginwindow's is per host, so it has no read-back.
	{Name: "Global.NSQuitAlwaysKeepsWindows", User: Agent, Argv: []string{defaultsTool, "write", "-g", "NSQuitAlwaysKeepsWindows", "-bool", "false"}, Step: Step,
		Check: &Check{Path: prefs + ".GlobalPreferences.plist", Key: "NSQuitAlwaysKeepsWindows", Want: []string{"0", "false"}}},
	{Name: "loginwindow.TALLogoutSavesState", User: Agent, Argv: []string{defaultsTool, "-currentHost", "write", "com.apple.loginwindow", "TALLogoutSavesState", "-bool", "false"},
		Optional: true, Step: Step},

	// Safari Safe Browsing off, which stops its traffic to Apple's lookup service. The key is unverified on
	// macOS 26, and a plain `defaults write` into Safari's container can exit 0 and change nothing when the
	// agent has no access to it, so the route reads the value back and skips the row if it did not land.
	// Stage 2 only.
	{Name: "safari-safe-browsing", User: Agent, Optional: true, Step: Step, Stage2Only: true, Protected: true,
		Argv: []string{defaultsTool, "write", safariPrefs, "WarnAboutFraudulentWebsites", "-bool", "false"},
		Read: &Read{Argv: []string{defaultsTool, "read", safariPrefs, "WarnAboutFraudulentWebsites"}, Want: "0"}},

	// Spotlight indexing off, and diagnostics never uploaded. The chmod and chgrp give the diagnostics file
	// the owner, group and mode System Settings gives it.
	{Name: "spotlight-off", User: Root, Argv: []string{"/usr/bin/mdutil", "-a", "-i", "off"}, Optional: true, Step: Step},
	def(Root, "CrashReporter.AutoSubmit", diagDomain, "AutoSubmit", kBool, "false", true),
	def(Root, "CrashReporter.ThirdPartyDataSubmit", diagDomain, "ThirdPartyDataSubmit", kBool, "false", true),
	{Name: "CrashReporter.mode", User: Root, Argv: []string{"/bin/chmod", "644", diagDomain + ".plist"}, Optional: true, Step: Step},
	{Name: "CrashReporter.group", User: Root, Argv: []string{"/usr/bin/chgrp", "admin", diagDomain + ".plist"}, Optional: true, Step: Step},
}
