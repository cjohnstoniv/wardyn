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
