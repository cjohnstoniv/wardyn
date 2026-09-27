// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

import (
	"os"
	"strings"
	"testing"
)

// TestFederatedTokenFile_RejectsUnsafeMode pins #1116: the federated token
// file is the workload-identity exact analogue of WARDYN_VAULT_K8S_TOKEN_FILE
// (#980/#1102), and must go through the same cliutil mode rule — a group- or
// world-writable file lets a local user substitute the credential wardynd
// authenticates to Azure Key Vault with.
func TestFederatedTokenFile_RejectsUnsafeMode(t *testing.T) {
	f := newFakeKV(t)
	file := writeFile(t, "projected-sa-1\n")
	for _, mode := range []os.FileMode{0o666, 0o620, 0o602, 0o644} {
		t.Run(mode.String(), func(t *testing.T) {
			if err := os.Chmod(file, mode); err != nil {
				t.Fatal(err)
			}
			// New() itself exchanges the federated token to prove the config
			// works, so the refusal surfaces here rather than on a later call.
			_, err := New(t.Context(), fakeConfig(f, file))
			if err == nil ||
				!strings.Contains(err.Error(), "WARDYN_AZURE_FEDERATED_TOKEN_FILE") ||
				!strings.Contains(err.Error(), file) {
				t.Fatalf("mode %04o: want a refusal naming the setting and path, got %v", mode, err)
			}
			if strings.Contains(err.Error(), "projected-sa-1") {
				t.Fatalf("mode %04o: refusal disclosed the token: %v", mode, err)
			}
		})
	}
}

// TestFederatedTokenFile_AcceptsSupportedModes proves the rule does not
// regress the delivery shapes a real workload-identity webhook produces.
func TestFederatedTokenFile_AcceptsSupportedModes(t *testing.T) {
	f := newFakeKV(t)
	file := writeFile(t, "projected-sa-1\n")
	for _, mode := range []os.FileMode{0o400, 0o440, 0o600, 0o640} {
		t.Run(mode.String(), func(t *testing.T) {
			if err := os.Chmod(file, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := New(t.Context(), fakeConfig(f, file)); err != nil {
				t.Fatalf("mode %04o: supported mode refused: %v", mode, err)
			}
		})
	}
}
