// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The owner's decisions are the zero value: warn only (no autonomy cap), env-var
// and file delivery allowed, the CC3 floor lifted for component credentials.
func TestComponentSettings_DefaultsAreTheOwnerDecisions(t *testing.T) {
	got := componentSettings(types.SiteConfig{})
	if got.AutonomyCap != "" {
		t.Errorf("autonomy_cap default = %q, want \"\" (no cap: warn only)", got.AutonomyCap)
	}
	if got.DenyResidentDelivery {
		t.Error("deny_resident_delivery default = true, want false (env-var and file delivery allowed)")
	}
	if got.RequireVaultForCredentials {
		t.Error("require_vault_for_credentials default = true, want false (CC3 floor lifted)")
	}
	set := types.ComponentSettings{AutonomyCap: types.AutonomyL1, DenyResidentDelivery: true, RequireVaultForCredentials: true}
	if got := componentSettings(types.SiteConfig{Components: &set}); got != set {
		t.Errorf("componentSettings = %+v, want the stored block %+v", got, set)
	}
}

// A document stored before the block existed decodes to a nil block, reads as the
// defaults, and re-encodes byte-for-byte: nothing in an old blob changes meaning.
func TestComponentSettings_FoldOldBlobWithoutBlock(t *testing.T) {
	old := `{"upstream_proxy_url":"http://proxy.corp.example:3128","scm_hosts":["github.com"],"sign_in_help_text":"Ask IT"}`
	var sc types.SiteConfig
	if err := json.Unmarshal([]byte(old), &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Components != nil {
		t.Fatalf("components = %+v, want nil for a blob without the key", sc.Components)
	}
	if got := componentSettings(sc); got != (types.ComponentSettings{}) {
		t.Errorf("componentSettings(old blob) = %+v, want the defaults", got)
	}
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != old {
		t.Errorf("re-encoded old blob = %s, want it byte-identical to %s", raw, old)
	}
}

func TestComponentSettings_ValidateAutonomyCap(t *testing.T) {
	for _, ok := range []types.AutonomyLevel{"", types.AutonomyL1, types.AutonomyL0} {
		if err := validateComponentSettings(&types.ComponentSettings{AutonomyCap: ok}); err != nil {
			t.Errorf("autonomy_cap %q refused: %v", ok, err)
		}
	}
	for _, bad := range []types.AutonomyLevel{types.AutonomyL2, types.AutonomyL3, "l1", "L1 ", "hold", "none"} {
		err := validateComponentSettings(&types.ComponentSettings{AutonomyCap: bad})
		if err == nil || !strings.Contains(err.Error(), "components.autonomy_cap") {
			t.Errorf("autonomy_cap %q: err = %v, want a refusal naming components.autonomy_cap", bad, err)
		}
	}
	if err := validateComponentSettings(nil); err != nil {
		t.Errorf("nil block refused: %v", err)
	}
}

func TestSiteConfigComponentSettings_PutRefusesBadCap(t *testing.T) {
	for _, body := range []string{
		`{"components":{"autonomy_cap":"L2"}}`,
		`{"components":{"autonomy_cap":"L3","deny_resident_delivery":true}}`,
		`{"components":{"autonomy_cap":"hold"}}`,
	} {
		fake := &fakeSiteConfigStore{}
		srv, _ := newSiteConfigHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "components.autonomy_cap") {
			t.Errorf("PUT %s = %d %s, want 400 naming components.autonomy_cap", body, w.Code, w.Body.String())
		}
		if fake.putSeen != nil {
			t.Errorf("PUT %s wrote %+v despite the refusal", body, fake.putSeen)
		}
	}
	fake := &fakeSiteConfigStore{}
	srv, _ := newSiteConfigHarness(t, fake)
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"components":{"autonomy_cap":"L1","unknown_switch":true}}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown key inside components = %d, want 400 (strict decode)", w.Code)
	}
}

func TestSiteConfigComponentSettings_PutAcceptsAndCarriesForward(t *testing.T) {
	stored := types.SiteConfig{
		ScmHosts:   []string{"github.com"},
		Components: &types.ComponentSettings{AutonomyCap: types.AutonomyL0, DenyResidentDelivery: true},
	}
	cases := []struct {
		name, body string
		start      types.SiteConfig
		want       *types.ComponentSettings
	}{
		{"L1 with both switches is stored", `{"components":{"autonomy_cap":"L1","deny_resident_delivery":true,"require_vault_for_credentials":true}}`,
			types.SiteConfig{}, &types.ComponentSettings{AutonomyCap: types.AutonomyL1, DenyResidentDelivery: true, RequireVaultForCredentials: true}},
		{"L0 is stored", `{"components":{"autonomy_cap":"L0"}}`, types.SiteConfig{}, &types.ComponentSettings{AutonomyCap: types.AutonomyL0}},
		{"a body that does not name the block carries it forward", `{"scm_hosts":["github.com"]}`, stored, stored.Components},
		{"an explicit {} clears it", `{"scm_hosts":["github.com"],"components":{}}`, stored, nil},
		{"an explicit null clears it", `{"components":null}`, stored, nil},
		{"an all-default block is stored as none", `{"components":{"autonomy_cap":"","deny_resident_delivery":false}}`, stored, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSiteConfigStore{cfg: tc.start}
			srv, _ := newSiteConfigHarness(t, fake)
			w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			got := fake.putSeen.Components
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("stored components = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Zero value == today: with no block stored, GET carries no components key, and a
// round trip of that GET through PUT stores none.
func TestSiteConfigComponentSettings_AbsentBlockIsByteIdentical(t *testing.T) {
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{ScmHosts: []string{"github.com"}}}
	srv, _ := newSiteConfigHarness(t, fake)
	get := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET = %d", get.Code)
	}
	if strings.Contains(get.Body.String(), `"components"`) {
		t.Errorf("GET with no block = %s, want no components key", get.Body.String())
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, get.Body.String()); w.Code != http.StatusOK {
		t.Fatalf("round-trip PUT = %d; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen.Components != nil {
		t.Errorf("round trip stored components = %+v, want none", fake.putSeen.Components)
	}
}
