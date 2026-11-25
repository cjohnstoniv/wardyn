// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// desktop_watermark on PUT: a body that names it stores it and audits it, a body that leaves it out keeps it, an
// all-default block is stored as none, and an install that never set it writes the row it always wrote.
func TestSiteConfigDesktopWatermarkPut(t *testing.T) {
	stored := types.SiteConfig{DesktopWatermark: &types.SiteDesktopWatermark{Required: true}}
	put := func(t *testing.T, fake *fakeSiteConfigStore, body string) *recRecorder {
		t.Helper()
		srv, audit := newSiteConfigHarness(t, fake)
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body); w.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d %s", body, w.Code, w.Body.String())
		}
		return audit
	}
	t.Run("a body that names the block stores it and audits it", func(t *testing.T) {
		fake := &fakeSiteConfigStore{}
		audit := put(t, fake, `{"desktop_watermark":{"required":true}}`)
		if got := fake.putSeen.DesktopWatermark; got == nil || !got.Required {
			t.Fatalf("stored desktop_watermark = %+v, want required", got)
		}
		d := siteFeaturesWriteDatum(t, audit.snapshot())
		if string(d["desktop_watermark_required"]) != "true" {
			t.Errorf("site_config.write desktop_watermark_required = %s, want true", d["desktop_watermark_required"])
		}
	})
	t.Run("a body that does not name the block keeps the stored one", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		audit := put(t, fake, `{}`)
		if got := fake.putSeen.DesktopWatermark; got == nil || !got.Required {
			t.Fatalf("stored desktop_watermark = %+v, want the required block carried forward", got)
		}
		d := siteFeaturesWriteDatum(t, audit.snapshot())
		if string(d["desktop_watermark_required"]) != "true" {
			t.Errorf("site_config.write desktop_watermark_required = %s, want the kept rule, true", d["desktop_watermark_required"])
		}
	})
	t.Run("an empty block is stored as none and a GET has no key", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		audit := put(t, fake, `{"desktop_watermark":{}}`)
		if fake.putSeen.DesktopWatermark != nil {
			t.Fatalf("stored desktop_watermark = %+v, want none", fake.putSeen.DesktopWatermark)
		}
		d := siteFeaturesWriteDatum(t, audit.snapshot())
		if string(d["desktop_watermark_required"]) != "false" {
			t.Errorf("site_config.write desktop_watermark_required = %s, want false for a named empty block", d["desktop_watermark_required"])
		}
		srv, _ := newSiteConfigHarness(t, fake)
		w := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["desktop_watermark"]; ok || strings.Contains(w.Body.String(), "desktop_watermark") {
			t.Errorf("GET carries desktop_watermark: %s", w.Body.String())
		}
	})
	t.Run("an install that never set it writes no desktop_watermark_required", func(t *testing.T) {
		audit := put(t, &fakeSiteConfigStore{}, `{}`)
		if v, ok := siteFeaturesWriteDatum(t, audit.snapshot())["desktop_watermark_required"]; ok {
			t.Errorf("site_config.write carries desktop_watermark_required = %s", v)
		}
	})
}
