// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// macos_vm.max_base_age_days on PUT: a negative number is refused, a body that does not name the block keeps it,
// and a body that names it replaces it.
func TestHandlePutSiteConfig_MacOSVM(t *testing.T) {
	stored := types.SiteConfig{MacOSVM: &types.SiteMacOSVM{MaxBaseAgeDays: 30}}
	put := func(t *testing.T, fake *fakeSiteConfigStore, body string) int {
		t.Helper()
		srv, _ := newSiteConfigHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
		if w.Code == http.StatusBadRequest && !strings.Contains(w.Body.String(), "macos_vm.max_base_age_days") {
			t.Errorf("refusal = %s, want it to name macos_vm.max_base_age_days", w.Body.String())
		}
		return w.Code
	}
	t.Run("a negative limit is refused and never stored", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		srv, _ := newSiteConfigHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"macos_vm":{"max_base_age_days":-1}}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "macos_vm.max_base_age_days: must be 0 or more — use 0 for no limit") {
			t.Fatalf("code = %d body = %s, want 400 with the approved sentence", w.Code, w.Body.String())
		}
		if fake.putSeen != nil {
			t.Error("a refused limit reached the store")
		}
	})
	t.Run("a body that does not name the block keeps the stored limit", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		if code := put(t, fake, `{}`); code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		if got := fake.putSeen.MacOSVM; got == nil || got.MaxBaseAgeDays != 30 {
			t.Errorf("stored macos_vm = %+v, want 30 days carried forward", got)
		}
	})
	t.Run("a body that names the block replaces it", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		if code := put(t, fake, `{"macos_vm":{"max_base_age_days":7}}`); code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		if got := fake.putSeen.MacOSVM; got == nil || got.MaxBaseAgeDays != 7 {
			t.Errorf("stored macos_vm = %+v, want 7 days", got)
		}
	})
	// The watch switch: a body that names the block without the key keeps the stored value, null counts as
	// omitted, and only an explicit true or false changes it.
	t.Run("allow_background_watch", func(t *testing.T) {
		off, on := false, true
		for _, tc := range []struct {
			name   string
			stored types.SiteConfig
			body   string
			want   *bool
		}{
			{"stored false, body names only the days: stays false", types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowBackgroundWatch: &off}},
				`{"macos_vm":{"max_base_age_days":7}}`, &off},
			{"stored false, body leaves the block out: stays false", types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowBackgroundWatch: &off}}, `{}`, &off},
			{"stored false, null switch is omitted: stays false", types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowBackgroundWatch: &off}},
				`{"macos_vm":{"allow_background_watch":null}}`, &off},
			{"stored false, null block: stays false", types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowBackgroundWatch: &off}}, `{"macos_vm":null}`, &off},
			{"stored false, explicit true re-enables", types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowBackgroundWatch: &off}},
				`{"macos_vm":{"allow_background_watch":true}}`, &on},
			{"stored true, explicit false disables", types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowBackgroundWatch: &on}},
				`{"macos_vm":{"allow_background_watch":false}}`, &off},
			{"fresh install, body names only the days: still allowed", types.SiteConfig{}, `{"macos_vm":{"max_base_age_days":7}}`, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fake := &fakeSiteConfigStore{cfg: tc.stored}
				if code := put(t, fake, tc.body); code != http.StatusOK {
					t.Fatalf("code = %d, want 200", code)
				}
				got := fake.putSeen.MacOSVM
				switch {
				case tc.want == nil && got != nil && got.AllowBackgroundWatch != nil:
					t.Errorf("stored switch = %v, want absent", *got.AllowBackgroundWatch)
				case tc.want != nil && (got == nil || got.AllowBackgroundWatch == nil || *got.AllowBackgroundWatch != *tc.want):
					t.Errorf("stored macos_vm = %+v, want allow_background_watch %v", got, *tc.want)
				}
				if tc.want == nil && !got.BackgroundWatchAllowed() {
					t.Error("a fresh install must read allowed")
				}
			})
		}
	})
}

// The kept-machine limits are refused at the boundary with whole sentences that name the field, and never reach
// the store. 0 or absent reads the default, so only a negative or too large number is refused.
func TestSiteConfigMacOSVMKeptLimitsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"idle days negative", `{"macos_vm":{"kept_machine_idle_days":-1}}`,
			"A kept machine can sit unused for 1 to 90 days before it is deleted. Set macos_vm.kept_machine_idle_days to a number from 1 to 90, or leave it out for 30."},
		{"idle days over 90", `{"macos_vm":{"kept_machine_idle_days":91}}`,
			"A kept machine can sit unused for 1 to 90 days before it is deleted. Set macos_vm.kept_machine_idle_days to a number from 1 to 90, or leave it out for 30."},
		{"per person negative", `{"macos_vm":{"max_kept_machines_per_person":-2}}`,
			"A person can keep 1 to 4 machines. Set macos_vm.max_kept_machines_per_person to a number from 1 to 4, or leave it out for 1."},
		{"per person over 4", `{"macos_vm":{"max_kept_machines_per_person":5}}`,
			"A person can keep 1 to 4 machines. Set macos_vm.max_kept_machines_per_person to a number from 1 to 4, or leave it out for 1."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSiteConfigStore{}
			srv, _ := newSiteConfigHarness(t, fake)
			w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, tc.body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("code = %d body = %s, want 400 with %q", w.Code, w.Body.String(), tc.want)
			}
			if fake.putSeen != nil {
				t.Error("a refused limit reached the store")
			}
		})
	}
	for _, body := range []string{
		`{"macos_vm":{"kept_machine_idle_days":1,"max_kept_machines_per_person":1}}`,
		`{"macos_vm":{"kept_machine_idle_days":90,"max_kept_machines_per_person":4}}`,
		`{"macos_vm":{"kept_machine_idle_days":0,"max_kept_machines_per_person":0}}`,
	} {
		fake := &fakeSiteConfigStore{}
		srv, _ := newSiteConfigHarness(t, fake)
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body); w.Code != http.StatusOK {
			t.Errorf("body %s: code = %d (%s), want 200", body, w.Code, w.Body.String())
		}
	}
}

// A save that omits the three kept-machine settings keeps what is stored, as the window switch does: the switch
// stays on (or off), and the limits do not fall back to their defaults. An explicit value changes them.
func TestSiteConfigMacOSVMKeptCarriedForward(t *testing.T) {
	on, off := true, false
	stored := func(sw *bool) types.SiteConfig {
		return types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowKeptMachines: sw, KeptMachineIdleDays: 60, MaxKeptMachinesPerPerson: 3}}
	}
	for _, tc := range []struct {
		name   string
		stored types.SiteConfig
		body   string
		sw     *bool
		days   int
		per    int
	}{
		{"stored on, body leaves the block out", stored(&on), `{}`, &on, 60, 3},
		{"stored on, null block", stored(&on), `{"macos_vm":null}`, &on, 60, 3},
		{"stored on, body names only the days", stored(&on), `{"macos_vm":{"max_base_age_days":7}}`, &on, 60, 3},
		{"stored on, null switch is omitted", stored(&on), `{"macos_vm":{"allow_kept_machines":null}}`, &on, 60, 3},
		{"stored off, body names only the days: stays off", stored(&off), `{"macos_vm":{"max_base_age_days":7}}`, &off, 60, 3},
		{"stored on, explicit false turns it off", stored(&on), `{"macos_vm":{"allow_kept_machines":false}}`, &off, 60, 3},
		{"stored off, explicit true turns it on", stored(&off), `{"macos_vm":{"allow_kept_machines":true}}`, &on, 60, 3},
		{"explicit limits replace the stored ones", stored(&on), `{"macos_vm":{"kept_machine_idle_days":7,"max_kept_machines_per_person":2}}`, &on, 7, 2},
		{"fresh install, body names only the days: still absent", types.SiteConfig{}, `{"macos_vm":{"max_base_age_days":7}}`, nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSiteConfigStore{cfg: tc.stored}
			srv, _ := newSiteConfigHarness(t, fake)
			if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, tc.body); w.Code != http.StatusOK {
				t.Fatalf("code = %d (%s), want 200", w.Code, w.Body.String())
			}
			got := fake.putSeen.MacOSVM
			if got == nil {
				t.Fatal("macos_vm not stored")
			}
			if (got.AllowKeptMachines == nil) != (tc.sw == nil) || (tc.sw != nil && *got.AllowKeptMachines != *tc.sw) {
				t.Errorf("stored allow_kept_machines = %v, want %v", got.AllowKeptMachines, tc.sw)
			}
			if got.KeptMachineIdleDays != tc.days || got.MaxKeptMachinesPerPerson != tc.per {
				t.Errorf("stored limits = %d days, %d per person; want %d, %d", got.KeptMachineIdleDays, got.MaxKeptMachinesPerPerson, tc.days, tc.per)
			}
		})
	}
}

// carryForwardSiteMacOSVM itself, without the handler: the kept-machine fields ride beside the window switch.
func TestSiteConfigMacOSVMKeptCarryForwardDirect(t *testing.T) {
	on := true
	existing := types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowKeptMachines: &on, KeptMachineIdleDays: 45, MaxKeptMachinesPerPerson: 2}}
	cfg := types.SiteConfig{}
	carryForwardSiteMacOSVM(&cfg, existing, map[string]bool{"macos_vm": true})
	if m := cfg.MacOSVM; m == nil || !m.KeptMachinesAllowed() || m.KeptMachineIdleDays != 45 || m.MaxKeptMachinesPerPerson != 2 {
		t.Fatalf("null block with only kept fields stored: carried %+v", cfg.MacOSVM)
	}
	cfg = types.SiteConfig{}
	carryForwardSiteMacOSVM(&cfg, existing, map[string]bool{})
	if cfg.MacOSVM != existing.MacOSVM {
		t.Fatalf("an unnamed block must keep the stored block, got %+v", cfg.MacOSVM)
	}
}
