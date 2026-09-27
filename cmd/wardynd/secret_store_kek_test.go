// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// WARDYN_KEK fails closed on every setting that cannot name a reachable key,
// and a Transit key over plain http:// to a remote host never boots.
func TestBuildKEK_FailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, addr, sel, key, want string }{
		{"unknown provider", "https://vault.example", "awskms", "wardyn", `WARDYN_KEK is "awskms"`},
		{"transit without a key", "https://vault.example", "transit", "", "needs WARDYN_VAULT_TRANSIT_KEY"},
		{"key without an address", "", "transit", "wardyn", "WARDYN_VAULT_ADDR is not"},
		{"plain http", "http://vault.example", "transit", "wardyn", "plain http://"},
		{"unreadable token file", "https://vault.example", "transit", "wardyn", "WARDYN_VAULT_TOKEN_FILE"},
		{"read-only key, no address", "", "local", "wardyn", "WARDYN_VAULT_ADDR is not"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := testExternalFlags(tc.addr, "")
			v.kek, v.transitMount, v.transitKey = strp(tc.sel), strp("transit"), strp(tc.key)
			k, _, err := buildKEK(t.Context(), v, "")
			if err == nil || k != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("buildKEK = (%v, %v); want a refusal containing %q", k, err, tc.want)
			}
		})
	}
	v, _ := testExternalFlags("", "")
	v.kek, v.transitMount, v.transitKey = strp("local"), strp("transit"), strp("")
	if k, writes, err := buildKEK(t.Context(), v, ""); k != nil || writes || err != nil {
		t.Fatalf("WARDYN_KEK=local with no Transit key = (%v, %v, %v); want no key service", k, writes, err)
	}
}

// TestRewrapMode_KeyServiceNeedsNoAgeKey: with WARDYN_KEK=transit, -rewrap
// needs no age key (every row may already be under the key service), so the
// age-key refusal gives way to the key service's own checks, which still run
// before the database is touched.
func TestRewrapMode_KeyServiceNeedsNoAgeKey(t *testing.T) {
	f := rekeyFlags("postgres://nobody@127.0.0.1:1/nope?connect_timeout=1", "", "")
	*f.vault.kek = kekTransit
	if err := rewrapMode(f); err == nil || !strings.Contains(err.Error(), "needs WARDYN_VAULT_TRANSIT_KEY") {
		t.Fatalf("-rewrap with WARDYN_KEK=transit and no Transit key = %v; want the key service's refusal", err)
	}
}
