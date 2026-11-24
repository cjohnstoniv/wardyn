// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"fmt"
	"slices"
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
		{"/usr/bin/sw_vers", "-productVersion"},
		{"/usr/bin/sw_vers", "-buildVersion"},
		{"/usr/bin/csrutil", "status"},
		{"/usr/sbin/spctl", "--status"},
		{"/usr/bin/profiles", "status", "-type", "enrollment"},
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
		{"/usr/bin/csrutil", "disable"},
		{"/usr/sbin/spctl", "--master-disable"},
		{"/usr/bin/sw_vers"},
		{"/usr/bin/profiles", "status", "-type", "bootstraptoken"},
	} {
		if err := ToolsExecAllowed(argv, t.TempDir()); err == nil {
			t.Errorf("%q allowed", argv)
		}
	}
}

// An app item from a zip or a disk image is unpacked or copied into its own stage, made root's there (links
// included) with no group or other write and no set-id bit, has its older copy in /Applications removed, and
// only then is copied in; the first step that fails is the answer and nothing after it runs. Every other form, including the admin profile's ditto, runs as given.
func TestToolsExecAppItemIsMadeRootsBeforeItsCopy(t *testing.T) {
	E := MacItemStageDir + "/edr"
	stage := E + "/" + itemAppStage
	for _, c := range []struct {
		name string
		argv []string
		want [][]string
	}{
		{"zip", []string{"/usr/bin/ditto", "-x", "-k", E + "/item.zip", MacApplications}, [][]string{
			{"/bin/rm", "-rf", stage},
			{"/usr/bin/ditto", "-x", "-k", E + "/item.zip", stage},
			{"/usr/sbin/chown", "-h", "-R", "-P", "root:wheel", stage},
			{"/bin/chmod", "-R", "-P", "go-w,ug-s", stage},
			{"/bin/rm", "-rf", MacApplications + "/Tool.app"},
			{"/usr/bin/ditto", stage, MacApplications},
		}},
		{"dmg", []string{"/usr/bin/ditto", E + "/mnt/Tool.app", MacApplications + "/Tool.app"}, [][]string{
			{"/bin/rm", "-rf", stage},
			{"/usr/bin/ditto", E + "/mnt/Tool.app", stage + "/Tool.app"},
			{"/usr/sbin/chown", "-h", "-R", "-P", "root:wheel", stage},
			{"/bin/chmod", "-R", "-P", "go-w,ug-s", stage},
			{"/bin/rm", "-rf", MacApplications + "/Tool.app"},
			{"/usr/bin/ditto", stage + "/Tool.app", MacApplications + "/Tool.app"},
		}},
		{"a package runs as given", []string{"/usr/sbin/installer", "-pkg", E + "/item.pkg", "-target", "/"}, [][]string{
			{"/usr/sbin/installer", "-pkg", E + "/item.pkg", "-target", "/"},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := newToolsRig(t)
			rig.t.listDir = func(dir string) ([]string, error) {
				if dir != stage {
					t.Errorf("listed %q, want the stage %q", dir, stage)
				}
				return []string{"Tool.app", "Helper.pkg", "Utilities"}, nil
			}
			if w := rig.exec(c.argv...); w.Code != 200 || !strings.Contains(w.Body.String(), `"exit_code":0`) {
				t.Fatalf("%q = %d %s, want 200 exit 0", c.argv, w.Code, w.Body)
			}
			if !slices.EqualFunc(rig.ran, c.want, slices.Equal) {
				t.Errorf("ran %q, want %q", rig.ran, c.want)
			}
		})
	}

	t.Run("a failed chown stops before the copy", func(t *testing.T) {
		rig := newToolsRig(t)
		var ran [][]string
		rig.t.listDir = func(string) ([]string, error) { return []string{"Tool.app"}, nil }
		rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
			ran = append(ran, argv)
			if argv[0] == "/usr/sbin/chown" {
				return []byte("chown: denied\n"), 1, nil
			}
			return nil, 0, nil
		}
		w := rig.exec("/usr/bin/ditto", "-x", "-k", E+"/item.zip", MacApplications)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"exit_code":1`) || !strings.Contains(w.Body.String(), "chown: denied") {
			t.Fatalf("answer = %d %s, want the chown's exit 1 and output", w.Code, w.Body)
		}
		if len(ran) != 3 || slices.ContainsFunc(ran, func(a []string) bool { return strings.HasPrefix(a[len(a)-1], MacApplications) }) {
			t.Errorf("ran %q, want rm, unpack, chown and no removal or copy in /Applications", ran)
		}
	})

	for _, argv := range [][]string{
		{"/usr/bin/ditto", "-x", "-k", E + "/item.zip", "/usr/local/bin"},
		{"/usr/bin/ditto", E + "/item.mobileconfig", AdminProfile("edr")},
		{"/usr/bin/ditto", E + "/mnt/Tool.app", MacApplications + "/Other.app"},
	} {
		if steps := appInstallSteps(argv); steps != nil {
			t.Errorf("appInstallSteps(%q) = %q, want nil", argv, steps)
		}
	}
}

// A push may write an item's staged file of its kind, or a file item's destination; never a harness's
// managed settings, Wardyn's own files or anything outside the roots.
func TestToolsPushBaseItems(t *testing.T) {
	tl := &Tools{}
	for _, dest := range []string{MacItemStageDir + "/edr/item.pkg", MacItemStageDir + "/a/item.dmg", MacItemStageDir + "/wifi/item.mobileconfig", MacItemStageDir + "/org-ca/item.cer", MacItemStageDir + "/edr/com.vendor.registration-token", "/Library/Application Support/Org/conf.json", "/opt/org/x"} {
		if err := tl.pushAllowed(dest); err != nil {
			t.Errorf("%s refused: %v", dest, err)
		}
	}
	for _, dest := range []string{MacItemStageDir + "/edr/mnt", MacItemStageDir + "/edr/.hidden", MacItemStageDir + "/edr/a/b", MacItemStageDir + "/item.pkg", MacItemStageDir + "/Edr/item.pkg",
		MacClaudeCodeManagedSettingsPath, strings.ToUpper(MacClaudeCodeManagedMCPPath), MacCodexRequirementsPath, MacRunDir + "/secret", MacSessionEnvPath,
		MacAgentBinaryPath, MacAgentPlistPath, "/Library/Keychains/System.keychain", "/Users/agent/x", "/etc/sudoers", "/private/etc/codex/requirements.toml", "/Library/../etc/codex/requirements.toml"} {
		if err := tl.pushAllowed(dest); err == nil {
			t.Errorf("%s allowed", dest)
		}
	}
}

// The trust-settings forms run as root like every other item form, with no authorization rule rewritten:
// each allowed form reaches run once with its argv, and the handler answers with the form's exit code, a
// refused trust (exit 1) included. No authorizationdb form can be run on its own.
func TestTrustSettingsFormsRunAsRoot(t *testing.T) {
	cer := ItemDir("org-ca") + "/" + ItemCert
	add := []string{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", MacSystemKeychain, cer}
	del := []string{"/usr/bin/security", "delete-certificate", "-t", "-Z", strings.Repeat("AB", 20), MacSystemKeychain}
	untrust := []string{"/usr/bin/security", "remove-trusted-cert", "-d", cer}
	for name, tc := range map[string]struct {
		argv []string
		exit int
	}{
		"trusted":       {argv: add},
		"trust refused": {argv: add, exit: 1},
		"removed":       {argv: del},
		"trust removed": {argv: untrust},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newToolsRig(t)
			var ran [][]string
			rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
				ran = append(ran, argv)
				return nil, tc.exit, nil
			}
			if w := rig.exec(tc.argv...); w.Code != 200 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"exit_code":%d`, tc.exit)) {
				t.Fatalf("status %d answer %s", w.Code, w.Body)
			}
			if len(ran) != 1 || !slices.Equal(ran[0], tc.argv) {
				t.Fatalf("run saw %q, want once %q", ran, tc.argv)
			}
		})
	}
	rig := newToolsRig(t)
	for _, argv := range [][]string{
		{"/usr/bin/security", "authorizationdb", "write", "com.apple.trust-settings.admin", "allow"},
		{"/usr/bin/security", "authorizationdb", "read", "com.apple.trust-settings.admin"},
		{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", MacSystemKeychain, cer},
		{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", MacSystemKeychain, "/tmp/x.cer"},
		{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustAsRoot", "-p", "ssl", "-k", MacSystemKeychain, cer},
		{"/usr/bin/security", "delete-certificate", "-t", "-Z", "abc", MacSystemKeychain},
		{"/usr/bin/security", "delete-certificate", "-c", "Org", MacSystemKeychain},
		{"/usr/bin/security", "verify-cert", "-c", cer},
		{"/usr/bin/security", "remove-trusted-cert", "-d", "/tmp/x.cer"},
		{"/usr/bin/security", "remove-trusted-cert", cer},
		{"/usr/bin/security", "remove-trusted-cert", "-d", cer, MacSystemKeychain},
		{"/usr/bin/security", "remove-trusted-cert", "-d", MacItemStageDir + "/../etc/" + ItemCert},
		{"/usr/bin/security", "remove-trusted-cert", "-d", ItemDir("..") + "/" + ItemCert},
	} {
		if w := rig.exec(argv...); w.Code != 403 {
			t.Errorf("%q: %d", argv, w.Code)
		}
	}
}

// edit-base's forms for wardyn-admin's copies take only its own directory and a valid item's copy there,
// never another account's home, another name or another source.
func TestToolsExecAdminProfileForms(t *testing.T) {
	D, E := MacAdminProfileDir, MacItemStageDir+"/wifi"
	for _, argv := range [][]string{
		{"/bin/rm", "-rf", D},
		{"/bin/mkdir", "-m", "0700", D},
		{"/usr/bin/ditto", E + "/item.mobileconfig", D + "/wifi.mobileconfig"},
		{"/usr/sbin/chown", "-h", "wardyn-admin", D},
		{"/usr/sbin/chown", "-h", "wardyn-admin", D + "/wifi.mobileconfig"},
		{"/usr/bin/stat", D},
		{"/usr/bin/stat", E},
	} {
		if err := ToolsExecAllowed(argv, t.TempDir()); err != nil {
			t.Errorf("%q refused: %v", argv, err)
		}
	}
	for _, argv := range [][]string{
		{"/bin/rm", "-rf", "/Users/wardyn-admin"},
		{"/bin/rm", "-rf", D + "/.."},
		{"/bin/mkdir", "-p", "-m", "0700", D},
		{"/bin/mkdir", "-m", "0755", D},
		{"/bin/mkdir", "-m", "0700", "/Users/agent/Wardyn"},
		{"/bin/mkdir", "-m", "0700", "/Users/wardyn-admin/Library/Wardyn"},
		{"/usr/bin/ditto", E + "/item.mobileconfig", "/Users/agent/Wardyn/wifi.mobileconfig"},
		{"/usr/bin/ditto", E + "/item.mobileconfig", "/Users/wardyn-admin/wifi.mobileconfig"},
		{"/usr/bin/ditto", E + "/item.mobileconfig", D + "/other.mobileconfig"},
		{"/usr/bin/ditto", E + "/item.pkg", D + "/wifi.mobileconfig"},
		{"/usr/bin/ditto", "/etc/sudoers", D + "/wifi.mobileconfig"},
		{"/usr/sbin/chown", "-h", "agent", D + "/wifi.mobileconfig"},
		{"/usr/sbin/chown", "wardyn-admin", D + "/wifi.mobileconfig"},
		{"/usr/sbin/chown", "-h", "wardyn-admin", D + "/../wifi.mobileconfig"},
		{"/usr/sbin/chown", "-h", "wardyn-admin", D + "/Wi Fi.mobileconfig"},
		{"/usr/sbin/chown", "-h", "wardyn-admin", "/Users/agent"},
		{"/usr/bin/stat", "/Users/agent"},
		{"/usr/bin/stat", "-f", "%N", D},
	} {
		if err := ToolsExecAllowed(argv, t.TempDir()); err == nil {
			t.Errorf("%q allowed", argv)
		}
	}
}
