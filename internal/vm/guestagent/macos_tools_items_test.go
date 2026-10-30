// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"strings"
	"testing"
)

// The base items' argv forms: each one allowed exactly, and a near miss of each refused.
func TestToolsExecBaseItemForms(t *testing.T) {
	S, E := MacItemStageDir, MacItemStageDir+"/edr"
	for _, argv := range [][]string{
		{"/bin/mkdir", "-p", "-m", "0700", S},
		{"/bin/mkdir", "-p", "-m", "0700", E},
		{"/bin/mkdir", "-p", "-m", "0755", "/Library/Application Support/Org"},
		{"/bin/rm", "-rf", E},
		{"/usr/sbin/installer", "-pkg", E + "/item.pkg", "-target", "/"},
		{"/usr/bin/ditto", "-x", "-k", E + "/item.zip", "/Applications"},
		{"/usr/bin/hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", E + "/mnt", E + "/item.dmg"},
		{"/usr/bin/ditto", E + "/mnt/Tool.app", "/Applications/Tool.app"},
		{"/usr/bin/hdiutil", "detach", "-force", E + "/mnt"},
		{"/usr/bin/profiles", "list"},
	} {
		if err := ToolsExecAllowed(argv, t.TempDir()); err != nil {
			t.Errorf("%q refused: %v", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"/bin/mkdir", "-p", "-m", "0777", E},
		{"/bin/mkdir", "-p", "-m", "0700", "/Library/Other"},
		{"/bin/mkdir", "-p", "-m", "0755", MacClaudeCodeManagedDir},
		{"/bin/rm", "-rf", S},
		{"/bin/rm", "-rf", "/"},
		{"/bin/rm", "-rf", E + "/../.."},
		{"/usr/sbin/installer", "-pkg", E + "/item.pkg", "-target", "/", "-allowUntrusted"},
		{"/usr/sbin/installer", "-pkg", "/tmp/x.pkg", "-target", "/"},
		{"/usr/sbin/installer", "-pkg", E + "/item.pkg", "-target", "/Volumes/x"},
		{"/usr/bin/ditto", "-x", "-k", E + "/item.zip", "/usr/local/bin"},
		{"/usr/bin/ditto", E + "/mnt/Tool.app", "/Applications/Other.app"},
		{"/usr/bin/ditto", "/etc/sudoers", "/Applications/Tool.app"},
		{"/usr/bin/ditto", E + "/mnt/Tool", "/Applications/Tool"},
		{"/usr/bin/hdiutil", "attach", "-nobrowse", "-readonly", "-noautoopen", "-mountpoint", "/Applications", E + "/item.dmg"},
		{"/usr/bin/hdiutil", "attach", "-nobrowse", "-noautoopen", "-readonly", "-mountpoint", E + "/mnt", E + "/item.dmg"},
		{"/usr/bin/hdiutil", "detach", "-force", "/"},
		{"/usr/bin/profiles", "list", "-all"},
		{"/usr/bin/profiles", "install", "-path", E + "/item.mobileconfig"},
		{"/usr/bin/profiles", "remove", "-all"},
	} {
		if err := ToolsExecAllowed(argv, t.TempDir()); err == nil {
			t.Errorf("%q allowed", argv)
		}
	}
}

// A push may write an item's staged file of its kind, or a file item's destination; never a harness's
// managed settings, Wardyn's own files or anything outside the roots.
func TestToolsPushBaseItems(t *testing.T) {
	tl := &Tools{}
	for _, dest := range []string{MacItemStageDir + "/edr/item.pkg", MacItemStageDir + "/a/item.dmg", MacItemStageDir + "/wifi/item.mobileconfig", "/Library/Application Support/Org/conf.json", "/opt/org/x"} {
		if err := tl.pushAllowed(dest); err != nil {
			t.Errorf("%s refused: %v", dest, err)
		}
	}
	for _, dest := range []string{MacItemStageDir + "/edr/other", MacItemStageDir + "/item.pkg", MacItemStageDir + "/Edr/item.pkg",
		MacClaudeCodeManagedSettingsPath, strings.ToUpper(MacClaudeCodeManagedMCPPath), MacCodexRequirementsPath, MacRunDir + "/secret",
		MacAgentBinaryPath, MacAgentPlistPath, "/Library/Keychains/System.keychain", "/Users/agent/x", "/etc/sudoers", "/private/etc/codex/requirements.toml", "/Library/../etc/codex/requirements.toml"} {
		if err := tl.pushAllowed(dest); err == nil {
			t.Errorf("%s allowed", dest)
		}
	}
}
