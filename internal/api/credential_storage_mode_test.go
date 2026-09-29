// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

// TestCredentialStorageMode pins design F-3's four-way enumeration: never a
// host, path or vault name — only which KIND of store. RED with any branch
// dropped or reordered (a Key Vault Describe() also has external != "", so
// the "Key Vault" prefix check must run FIRST).
func TestCredentialStorageMode(t *testing.T) {
	for _, c := range []struct {
		name, external, keyService, want string
	}{
		{"local", "", "", "local"},
		{"key service (Vault Transit)", "", "Vault Transit at vault.example:8200", "key_service"},
		{"vault store mode", "Vault at vault.example:8200", "", "vault"},
		{"key vault store mode", "Key Vault my-vault", "", "key_vault"},
		// A key service is never reported while store mode also is (pg's own
		// KeyService() zeroes under s.writeExt) — store mode wins if both are
		// somehow set, since that is the KIND that actually holds the value.
		{"store mode wins over a stray key service value", "Vault at vault.example:8200", "Vault Transit at vault.example:8200", "vault"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := credentialStorageMode(c.external, c.keyService); got != c.want {
				t.Errorf("credentialStorageMode(%q, %q) = %q, want %q", c.external, c.keyService, got, c.want)
			}
		})
	}
}

// TestSetupStatus_CredentialStorageSurvivesMemberRedaction pins design F-3:
// the field is kept through redactSetupStatusForUser (unlike Checks, which
// carries the SAME fact as admin-only diagnostic detail) — every signed-in
// person is told which kind of store their own credential goes into. RED if
// redaction ever starts zeroing it.
func TestSetupStatus_CredentialStorageSurvivesMemberRedaction(t *testing.T) {
	full := SetupStatus{CredentialStorage: "key_vault"}
	got := redactSetupStatusForUser(full)
	if got.CredentialStorage != "key_vault" {
		t.Fatalf("redacted CredentialStorage = %q, want it kept (\"key_vault\")", got.CredentialStorage)
	}
}
