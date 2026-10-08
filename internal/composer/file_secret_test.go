// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func fileSecretGrant(file, secretName string) types.GrantSpec {
	sc, _ := json.Marshal(map[string]string{"file": file, "secret_name": secretName})
	return types.GrantSpec{Kind: types.GrantFileSecret, Scope: sc}
}

// file_secret is matched like env_secret: the FILE name in the host slot, so a
// ceiling pairing is exact on (file, secret) and an operator-blessed secret
// cannot be re-homed under a file name the operator never wrote.
func TestGrantPairing_FileSecret(t *testing.T) {
	host, ref, kh, covered, ok := GrantPairing(fileSecretGrant("api-token", "corp-token"))
	if host != "api-token" || ref != "corp-token" || kh != "" || !covered || !ok {
		t.Fatalf("GrantPairing = (%q, %q, %q, %v, %v), want (api-token, corp-token, \"\", true, true)", host, ref, kh, covered, ok)
	}
	if _, _, _, covered, ok := GrantPairing(types.GrantSpec{Kind: types.GrantFileSecret, Scope: json.RawMessage(`{"file":"x"}`)}); !covered || ok {
		t.Errorf("a scope with no secret_name: covered=%v ok=%v, want covered and NOT ok (matches nothing)", covered, ok)
	}
	ceiling := []types.GrantSpec{fileSecretGrant("api-token", "corp-token")}
	for _, tc := range []struct {
		g    types.GrantSpec
		want bool
	}{
		{fileSecretGrant("api-token", "corp-token"), true},
		{fileSecretGrant("other-name", "corp-token"), false},
		{fileSecretGrant("api-token", "prod-db-password"), false},
		{types.GrantSpec{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"api-token","secret_name":"corp-token"}`)}, false},
	} {
		if got := PairingInCeiling(tc.g, ceiling); got != tc.want {
			t.Errorf("PairingInCeiling(%s %s) = %v, want %v", tc.g.Kind, tc.g.Scope, got, tc.want)
		}
	}
}

// A file_secret is graded like env_secret: HIGH, and the rationale says the
// value is resident for the whole run.
func TestGrade_FileSecretIsHigh(t *testing.T) {
	items := Grade(RunInput{}, types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{fileSecretGrant("api-token", "corp-token")}})
	it, ok := find(items, "eligible_grants[0]")
	if !ok {
		t.Fatalf("no risk item for the file_secret grant: %+v", items)
	}
	if it.Level != RiskHigh || it.Value != "file_secret" || !strings.Contains(it.Rationale, "WHOLE run") {
		t.Errorf("file_secret graded %+v, want HIGH naming the whole-run residency", it)
	}
}
