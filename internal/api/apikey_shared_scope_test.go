// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// `shared` pins the injection sink to the OPERATOR's namespace; only the
// component gate may set it, for an org component's provided secret. Every
// authored door refuses it: the strict decode (stored, inline, preset,
// profile ceiling), the deployment default file, and the lenient decode
// record-mode synthesis uses.
func TestValidateEligibleGrant_RefusesSharedAPIKey_StrictAndLenient(t *testing.T) {
	scope := func(shared any) json.RawMessage {
		m := map[string]any{"host": "api.example.com", "header": "Authorization", "format": "Bearer %s", "secret_name": "corp-token", "require_tls": true}
		if shared != nil {
			m["shared"] = shared
		}
		return mustJSON(m)
	}
	spec := func(sc json.RawMessage) types.RunPolicySpec {
		return types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: sc}}}
	}
	const want = "shared is set by Wardyn for an org component, never authored"

	shared := spec(scope(true))
	for door, err := range map[string]error{
		"strict":  validatePolicySpec(shared),
		"lenient": validatePolicySpecLenient(shared),
		"grant":   validateEligibleGrant(0, shared.EligibleGrants[0]),
		"default": loadPolicySpecFrom(t, shared),
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s door: err = %v, want %q", door, err, want)
		}
	}
	for _, sc := range []json.RawMessage{scope(nil), scope(false)} {
		if err := validatePolicySpec(spec(sc)); err != nil {
			t.Errorf("scope %s: %v, want accepted", sc, err)
		}
		if err := validatePolicySpecLenient(spec(sc)); err != nil {
			t.Errorf("scope %s (lenient): %v, want accepted", sc, err)
		}
	}

	// The strict decode the sink runs accepts the declared field and carries
	// none of it onto the rule; the helper is the one reader.
	if _, err := injectionRuleFromScope(scope(true)); err != nil {
		t.Errorf("injectionRuleFromScope refused a declared field: %v", err)
	}
	for sc, wantShared := range map[string]bool{string(scope(true)): true, string(scope(false)): false, string(scope(nil)): false, `not json`: false, `{"shared":"yes"}`: false} {
		if got := apiKeyScopeShared(json.RawMessage(sc)); got != wantShared {
			t.Errorf("apiKeyScopeShared(%s) = %v, want %v", sc, got, wantShared)
		}
	}
}

// loadPolicySpecFrom writes spec where WARDYN_DEFAULT_POLICY would point and
// loads it through the default-policy door.
func loadPolicySpecFrom(t *testing.T, spec types.RunPolicySpec) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), "default.json")
	if err := os.WriteFile(p, mustJSON(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPolicySpec(p)
	return err
}

// The sink reads a shared grant from the operator's namespace and nowhere
// else, whatever the run's owner, owner_only or operator ownership say; every
// other grant keeps grantReadOwner's rule. Scope-only: no store is consulted.
func TestInjectionGrantRead_SharedPinsTheOperatorNamespace(t *testing.T) {
	shared := json.RawMessage(`{"host":"api.example.com","secret_name":"corp-token","shared":true}`)
	plain := json.RawMessage(`{"host":"api.example.com","secret_name":"corp-token"}`)
	for _, c := range []struct {
		name                     string
		scope                    json.RawMessage
		ownerOnly, operatorOwned bool
		owner                    string
		ownRowOnly               bool
	}{
		{"shared, a person's run", shared, false, false, "", true},
		{"shared, owner_only set anyway", shared, true, false, "", true},
		{"shared, an operator's run", shared, false, true, "", true},
		{"plain, a person's run", plain, false, false, "alice@example.com", false},
		{"plain owner_only, a person's run", plain, true, false, "alice@example.com", true},
		{"plain owner_only, an operator's run", plain, true, true, "", true},
		{"an unreadable scope is not shared", json.RawMessage(`nope`), false, false, "alice@example.com", false},
	} {
		owner, own := injectionGrantRead(c.scope, "alice@example.com", c.ownerOnly, c.operatorOwned)
		if owner != c.owner || own != c.ownRowOnly {
			t.Errorf("%s: injectionGrantRead = (%q, %v), want (%q, %v)", c.name, owner, own, c.owner, c.ownRowOnly)
		}
	}
}
