// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// posture reads FileVault with `fdesetup status` only: on and off as it prints them, and "" (unknown, never
// off) for a failed run, a nonzero exit or any other words. The call is the posture's own, not an allowed
// exec argv.
func TestToolsPostureFileVault(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		code int
		err  error
		want string
	}{
		{"on", "FileVault is On.\n", 0, nil, FileVaultOn},
		{"off", "FileVault is Off.\n", 0, nil, FileVaultOff},
		{"on while encrypting", "FileVault is On.\nEncryption in progress: Percent completed = 12\n", 0, nil, FileVaultOn},
		{"off with deferred enablement", "FileVault is Off.\nDeferred enablement appears to be active for user 'agent'.\n", 0, nil, FileVaultOff},
		{"other words", "FileVault might be on\n", 0, nil, ""},
		{"a later line says on", "nothing\nFileVault is On.\n", 0, nil, ""},
		{"nonzero exit", "FileVault is Off.\n", 1, nil, ""},
		{"run failed", "", 0, errors.New("no such tool"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newToolsRig(t)
			rig.t.sshAddr = "127.0.0.1:1"
			var ran [][]string
			rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
				ran = append(ran, argv)
				if argv[0] == "/usr/bin/fdesetup" {
					return []byte(tc.out), tc.code, tc.err
				}
				return []byte("agent 502\nroot 0\n"), 0, nil
			}
			w := rig.do("GET", "/v1/tools/posture", "")
			var p ToolsPosture
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.FileVault != tc.want {
				t.Fatalf("posture = %d %s, want file_vault %q", w.Code, w.Body, tc.want)
			}
			if !slices.ContainsFunc(ran, func(a []string) bool { return slices.Equal(a, []string{"/usr/bin/fdesetup", "status"}) }) {
				t.Errorf("ran %v, want /usr/bin/fdesetup status", ran)
			}
		})
	}
	var old ToolsPosture
	if err := json.Unmarshal([]byte(`{"accounts":[],"remote_login":false}`), &old); err != nil || old.FileVault != "" {
		t.Errorf("an older agent's posture = %+v %v", old, err)
	}
	if b, _ := json.Marshal(ToolsPosture{}); strings.Contains(string(b), "file_vault") {
		t.Errorf("an unread field is sent: %s", b)
	}
	// No exec allowlist change: fdesetup is not an allowed exec argv.
	if err := ToolsExecAllowed([]string{"/usr/bin/fdesetup", "status"}, t.TempDir()); !errors.Is(err, ErrToolsRefused) {
		t.Errorf("ToolsExecAllowed(fdesetup status) = %v, want ErrToolsRefused", err)
	}
}
