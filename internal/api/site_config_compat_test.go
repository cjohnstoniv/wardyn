// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// siteConfigKeysV066 is the SiteConfig vocabulary of v0.6.6 — the last release
// whose SDK/CLI is documented to GET this whole document, decode it into its own
// struct, and PUT the result back. FROZEN: it describes a shipped binary, not
// this tree.
var siteConfigKeysV066 = []string{
	"upstream_proxy_secret_ref", "upstream_proxy_url", "artifact_overrides",
	"egress_redirects", "scm_hosts", "integrations",
}

// TestSiteConfigRoundTripKeepsFieldsAnOlderClientCannotName is F285.
//
// PUT /site-config is a whole-document replace, so a v0.6.6 `site-config get |
// edit | apply` round trip re-marshals a struct that has no field for anything
// 0.7 added — and the newer keys are simply absent from the body. The handler
// could not tell that from "cleared", so internal_hosts and
// upstream_proxy_no_proxy were silently erased, with no warning on either side.
// The identical footgun was solved by hand twice before, for integrations and
// onboarding_completed_at; these two arrived afterwards and were not.
func TestSiteConfigRoundTripKeepsFieldsAnOlderClientCannotName(t *testing.T) {
	stored := types.SiteConfig{
		UpstreamProxyURL:     "http://proxy.corp.example:3128",
		ScmHosts:             []string{"github.example.com"},
		UpstreamProxyNoProxy: []string{"bedrock-runtime.us-east-1.amazonaws.com"},
		InternalHosts: []types.InternalHost{{
			HostSuffix: "bedrock-runtime.us-east-1.amazonaws.com", CIDRs: []string{"10.0.0.0/8"},
		}},
	}

	// EXACTLY what a v0.6.6 client emits: the stored document decoded into that
	// release's struct and re-marshalled. Nothing malicious, nothing malformed
	// — the documented round trip.
	v066Body := `{"upstream_proxy_url":"http://proxy.corp.example:3128","scm_hosts":["github.example.com"]}`

	t.Run("a v0.6.6 round-trip PUT preserves both", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		srv, _ := newSiteConfigHarness(t, fake)

		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, v066Body)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if fake.putSeen == nil {
			t.Fatal("nothing was written")
		}
		if len(fake.putSeen.InternalHosts) != 1 {
			t.Errorf("internal_hosts = %v, want the stored value carried forward — an older client's documented "+
				"GET-then-PUT erased an operator-authored field it cannot even name", fake.putSeen.InternalHosts)
		}
		if !reflect.DeepEqual(fake.putSeen.UpstreamProxyNoProxy, stored.UpstreamProxyNoProxy) {
			t.Errorf("upstream_proxy_no_proxy = %v, want %v carried forward",
				fake.putSeen.UpstreamProxyNoProxy, stored.UpstreamProxyNoProxy)
		}
		// The fields the older client DOES name are still authoritative — this
		// is a carry-forward for silence, not a merge.
		if fake.putSeen.UpstreamProxyURL != "http://proxy.corp.example:3128" || len(fake.putSeen.ScmHosts) != 1 {
			t.Errorf("the body's own fields were not written: %+v", fake.putSeen)
		}
	})

	// A CARRY-FORWARD THAT CANNOT BE UNDONE IS A WORSE BUG. A 0.7 client that
	// MENTIONS the field clears it, both spellings.
	for _, tc := range []struct{ name, body string }{
		{"an explicit empty array clears", `{"internal_hosts":[],"upstream_proxy_no_proxy":[]}`},
		{"an explicit null clears", `{"internal_hosts":null,"upstream_proxy_no_proxy":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSiteConfigStore{cfg: stored}
			srv, _ := newSiteConfigHarness(t, fake)

			w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if len(fake.putSeen.InternalHosts) != 0 || len(fake.putSeen.UpstreamProxyNoProxy) != 0 {
				t.Errorf("a body that NAMES the field did not clear it (internal_hosts=%v no_proxy=%v) — only "+
					"silence may be read as silence, or an operator can never remove an entry",
					fake.putSeen.InternalHosts, fake.putSeen.UpstreamProxyNoProxy)
			}
		})
	}

	// THE ANTI-FORGETTING HALF, and the reason this is a rule rather than two
	// more hand-rescued fields: the NEXT key added to types.SiteConfig has the
	// same footgun, and nothing would have caught it. Every key must sit on one
	// declared side of the v0.6.6 line.
	t.Run("every SiteConfig key has a decided compatibility side", func(t *testing.T) {
		serverOwned := []string{"integrations", "onboarding_completed_at"} // refused on PUT, carried from the store
		typ := reflect.TypeOf(types.SiteConfig{})
		for i := range typ.NumField() {
			key, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if key == "" || key == "-" {
				continue
			}
			switch {
			case contains(siteConfigKeysV066, key), contains(serverOwned, key),
				contains(siteConfigFieldsAfter066, key):
				continue
			}
			t.Errorf("types.SiteConfig gained %q and nothing decided what a client that cannot name it should do "+
				"to it. Add it to siteConfigFieldsAfter066 (site_config.go) so an older client's silence carries "+
				"it forward, or to serverOwned here if the PUT refuses it.", key)
		}
	})

	// And the carry-forward list itself must name real keys.
	t.Run("the carry-forward list names real SiteConfig keys", func(t *testing.T) {
		var keys []string
		typ := reflect.TypeOf(types.SiteConfig{})
		for i := range typ.NumField() {
			key, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			keys = append(keys, key)
		}
		for _, k := range siteConfigFieldsAfter066 {
			if !contains(keys, k) {
				t.Errorf("siteConfigFieldsAfter066 names %q, which types.SiteConfig no longer has", k)
			}
			if contains(siteConfigKeysV066, k) {
				t.Errorf("%q is in the v0.6.6 vocabulary — an older client CAN name it, so its absence is a decision", k)
			}
		}
	})

	// The response body still round-trips: the carried-forward value is what a
	// caller reads back, so a 0.7 client is not told the field vanished.
	t.Run("the response reflects the carried-forward document", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		srv, _ := newSiteConfigHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, v066Body)
		var got types.SiteConfig
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.InternalHosts) != 1 {
			t.Errorf("the PUT response dropped internal_hosts (%+v) — a client reading its own write back would "+
				"conclude the field was erased", got)
		}
	})
}
