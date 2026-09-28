// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package cliutil

import (
	"os"
	"strings"
	"testing"
)

// The delivery shapes a supported mechanism produces must be read; the
// hand-made host file the process's own uid owns and anyone can read must not.
func TestSecretFile_ModeRuleByOwner(t *testing.T) {
	const euid = 65532
	for _, tc := range []struct {
		name   string
		perm   os.FileMode
		owner  int
		refuse bool
	}{
		{"kubelet Secret volume, root 0440 under fsGroup", 0o440, 0, false},
		{"Secrets Store CSI, root 0644", 0o644, 0, false},
		{"Vault Agent default, uid 100 0644", 0o644, 100, false},
		{"own file 0600", 0o600, euid, false},
		{"own file 0640", 0o640, euid, false},
		{"own file 0644", 0o644, euid, true},
		{"root-owned group-writable 0460", 0o460, 0, true},
	} {
		err := CheckSecretFileMode("WARDYN_TEST_STR_FILE", "/p", tc.perm, tc.owner, euid)
		if (err != nil) != tc.refuse {
			t.Errorf("%s: err = %v, want refuse=%v", tc.name, err, tc.refuse)
		}
	}
	// Running as root, nothing is "the process's own" — root can read anything.
	if err := CheckSecretFileMode("WARDYN_TEST_STR_FILE", "/p", 0o644, 0, 0); err != nil {
		t.Errorf("euid 0, root-owned 0644: %v", err)
	}
	err := CheckSecretFileMode("WARDYN_TEST_STR_FILE", "/p", 0o644, euid, euid)
	if err == nil || !strings.Contains(err.Error(), "chmod 640") || !strings.Contains(err.Error(), "agent-inject-perms") {
		t.Fatalf("own 0644 refusal must name chmod 640 and agent-inject-perms, got %v", err)
	}
}
