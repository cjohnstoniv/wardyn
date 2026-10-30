// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
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
		{"/bin/mkdir", "-p", "-m", "0755", MacProfileOffer},
		{"/usr/bin/ditto", E + "/item.mobileconfig", MacProfileOffer + "/edr.mobileconfig"},
		{"/bin/rm", "-rf", MacProfileOffer},
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
		{"/bin/mkdir", "-p", "-m", "0700", MacProfileOffer},
		{"/usr/bin/ditto", E + "/item.mobileconfig", MacProfileOffer + "/other.mobileconfig"},
		{"/usr/bin/ditto", E + "/item.mobileconfig", "/Library/LaunchDaemons/edr.plist"},
		{"/usr/bin/ditto", E + "/item.pkg", MacProfileOffer + "/edr.mobileconfig"},
		{"/usr/bin/ditto", "/etc/sudoers", MacProfileOffer + "/edr.mobileconfig"},
		{"/bin/rm", "-rf", MacProfileOffer + "/.."},
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
	for _, dest := range []string{MacItemStageDir + "/edr/item.pkg", MacItemStageDir + "/a/item.dmg", MacItemStageDir + "/wifi/item.mobileconfig", MacItemStageDir + "/org-ca/item.cer", MacItemStageDir + "/edr/com.vendor.registration-token", "/Library/Application Support/Org/conf.json", "/opt/org/x"} {
		if err := tl.pushAllowed(dest); err != nil {
			t.Errorf("%s refused: %v", dest, err)
		}
	}
	for _, dest := range []string{MacItemStageDir + "/edr/mnt", MacItemStageDir + "/edr/.hidden", MacItemStageDir + "/edr/a/b", MacItemStageDir + "/item.pkg", MacItemStageDir + "/Edr/item.pkg",
		MacClaudeCodeManagedSettingsPath, strings.ToUpper(MacClaudeCodeManagedMCPPath), MacCodexRequirementsPath, MacRunDir + "/secret",
		MacAgentBinaryPath, MacAgentPlistPath, "/Library/Keychains/System.keychain", "/Users/agent/x", "/etc/sudoers", "/private/etc/codex/requirements.toml", "/Library/../etc/codex/requirements.toml"} {
		if err := tl.pushAllowed(dest); err == nil {
			t.Errorf("%s allowed", dest)
		}
	}
}

// The trust-settings forms run inside the authorization bracket, in this order: authorizationdb read
// (saved), write allow, the form, write the saved rule back. The restore runs on every path: the form
// failing, the allow failing, the request cancelled. A read that fails changes nothing, and a restore
// that fails is an error. verify-cert and every other form run without it, and no authorizationdb form
// can be run on its own.
func TestTrustSettingsBracketRestoresOnEveryPath(t *testing.T) {
	const saved = "<?xml?><plist><dict><key>rule</key></dict></plist>"
	cer := ItemDir("org-ca") + "/" + ItemCert
	add := []string{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", MacSystemKeychain, cer}
	del := []string{"/usr/bin/security", "delete-certificate", "-t", "-Z", strings.Repeat("AB", 20), MacSystemKeychain}
	for name, tc := range map[string]struct {
		argv               []string
		readErr, allowErr  error
		restoreErr         error
		formExit           int
		cancel             bool
		wantSteps          string
		wantStatus, wantEx int
	}{
		"trusted":           {argv: add, wantSteps: "read|allow|form|restore", wantStatus: 200},
		"removed":           {argv: del, wantSteps: "read|allow|form|restore", wantStatus: 200},
		"the form fails":    {argv: add, formExit: 1, wantSteps: "read|allow|form|restore", wantStatus: 200, wantEx: 1},
		"the allow fails":   {argv: add, allowErr: errors.New("denied"), wantSteps: "read|allow|restore", wantStatus: 500},
		"cancelled":         {argv: add, cancel: true, wantSteps: "read|allow|form|restore", wantStatus: 500},
		"the read fails":    {argv: add, readErr: errors.New("no"), wantSteps: "read", wantStatus: 500},
		"the restore fails": {argv: add, restoreErr: errors.New("io"), wantSteps: "read|allow|form|restore", wantStatus: 500},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newToolsRig(t)
			var steps []string
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rig.t.run = func(c context.Context, argv []string) ([]byte, int, error) {
				steps = append(steps, "form")
				if tc.cancel {
					cancel()
					return nil, 0, c.Err()
				}
				return nil, tc.formExit, nil
			}
			rig.t.authdb = func(c context.Context, args []string, stdin []byte) ([]byte, error) {
				switch {
				case slices.Equal(args, []string{"read", trustSettingsRight}):
					steps = append(steps, "read")
					return []byte(saved), tc.readErr
				case slices.Equal(args, []string{"write", trustSettingsRight, "allow"}):
					steps = append(steps, "allow")
					return nil, tc.allowErr
				case slices.Equal(args, []string{"write", trustSettingsRight}) && string(stdin) == saved && c.Err() == nil:
					steps = append(steps, "restore")
					return nil, tc.restoreErr
				}
				t.Fatalf("authorizationdb %q stdin %q ctx %v", args, stdin, c.Err())
				return nil, nil
			}
			b := `{"argv":[` + strings.Join(quoteAll(tc.argv), ",") + `]}`
			w := httptest.NewRecorder()
			rig.t.ToolsHandler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/tools/exec", strings.NewReader(b)).WithContext(ctx))
			if got := strings.Join(steps, "|"); got != tc.wantSteps || w.Code != tc.wantStatus {
				t.Fatalf("steps %s status %d (%s), want %s %d", got, w.Code, w.Body, tc.wantSteps, tc.wantStatus)
			}
			if tc.wantStatus == 200 && !strings.Contains(w.Body.String(), fmt.Sprintf(`"exit_code":%d`, tc.wantEx)) {
				t.Fatalf("answer %s", w.Body)
			}
		})
	}
	rig := newToolsRig(t)
	rig.t.authdb = func(context.Context, []string, []byte) ([]byte, error) {
		t.Fatal("verify-cert ran in the bracket")
		return nil, nil
	}
	if w := rig.exec("/usr/bin/security", "verify-cert", "-L", "-p", "ssl", "-c", cer); w.Code != 200 {
		t.Fatalf("verify-cert: %d %s", w.Code, w.Body)
	}
	for _, argv := range [][]string{
		{"/usr/bin/security", "authorizationdb", "write", trustSettingsRight, "allow"},
		{"/usr/bin/security", "authorizationdb", "read", trustSettingsRight},
		{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", MacSystemKeychain, cer},
		{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", MacSystemKeychain, "/tmp/x.cer"},
		{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustAsRoot", "-p", "ssl", "-k", MacSystemKeychain, cer},
		{"/usr/bin/security", "delete-certificate", "-t", "-Z", "abc", MacSystemKeychain},
		{"/usr/bin/security", "delete-certificate", "-c", "Org", MacSystemKeychain},
		{"/usr/bin/security", "verify-cert", "-c", cer},
	} {
		if w := rig.exec(argv...); w.Code != 403 {
			t.Errorf("%q: %d", argv, w.Code)
		}
	}
}
