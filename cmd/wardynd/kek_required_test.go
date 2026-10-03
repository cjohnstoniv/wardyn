// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// custodyStore reports the two descriptions refuseKEKRequired reads.
type custodyStore struct {
	secretstore.Store
	external, keyService string
}

func (c custodyStore) StoresExternally() string { return c.external }
func (c custodyStore) KeyService() string       { return c.keyService }

// The refusal depends on who holds the credentials, nothing else: a set,
// ephemeral or absent WARDYN_AGE_KEY and an OIDC issuer never reach it.
func TestRefuseKEKRequired(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required bool
		store    custodyStore
		refuse   bool
	}{
		{"local key, flag set (a set or an ephemeral age key, with or without OIDC)", true, custodyStore{}, true},
		{"local key, flag unset", false, custodyStore{}, false},
		{"key service, flag set", true, custodyStore{keyService: "Vault Transit at vault.example:8200"}, false},
		{"store mode, flag set", true, custodyStore{external: "Vault at vault.example:8200"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseKEKRequired(tc.required, tc.store)
			if tc.refuse != (err != nil) {
				t.Fatalf("refuseKEKRequired = %v; want refusal %v", err, tc.refuse)
			}
			if err != nil {
				for _, want := range []string{"refusing to start:", "WARDYN_KEK=transit|azurekv", "wardynd -rewrap"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("refusal %q lacks %q", err, want)
					}
				}
			}
		})
	}
}

// -rewrap, the remedy, never reaches the check: it builds its store through
// newSecretStore, so it still runs with the flag set.
func TestRewrapMode_RunsWithKEKRequired(t *testing.T) {
	f := rekeyFlags("postgres://nobody@127.0.0.1:1/nope?connect_timeout=1", "", "")
	*f.vault.kek = kekTransit
	f.vault.kekRequired = new(bool)
	*f.vault.kekRequired = true
	err := rewrapMode(f)
	if err == nil || strings.Contains(err.Error(), "WARDYN_KEK_REQUIRED") || !strings.Contains(err.Error(), "needs WARDYN_VAULT_TRANSIT_KEY") {
		t.Fatalf("-rewrap with WARDYN_KEK_REQUIRED set = %v; want the key service's own refusal, not the custody one", err)
	}
}
