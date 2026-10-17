// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

// The default key domain names where its key is: the credential key service when one is configured, else
// the deployment's own key.
func TestMiscCovDefaultKeyDomainKey(t *testing.T) {
	srv := newHarness(t).srv
	if got := srv.defaultKeyDomainKey(); got != "Credential key" {
		t.Errorf("with no key service = %q, want %q", got, "Credential key")
	}
	srv.cfg.SecretKeyService = "Azure Key Vault"
	if got := srv.defaultKeyDomainKey(); got != "Azure Key Vault" {
		t.Errorf("with a key service = %q, want it named", got)
	}
}
