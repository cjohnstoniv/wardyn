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
}
