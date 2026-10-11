// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestSiteConfigCaptureRestoresThroughTheSDKWhateverTheStoredBaseline pins the documented
// `site-config get` / `site-config set` round trip against the real handler. PUT /site-config refuses a
// body that changes the egress baseline, so a capture restored after a reset (nothing stored), or after
// the security tier moved the baseline, used to be refused whole. The SDK now sends the document
// without the baseline: the restore lands, the stored baseline stays exactly as it was, and the
// result says the file's baseline was not applied. The server's refusal for any other caller stays.
func TestSiteConfigCaptureRestoresThroughTheSDKWhateverTheStoredBaseline(t *testing.T) {
	captured := types.SiteConfig{
		UpstreamProxyURL: "http://proxy.corp.example:3128",
		Egress:           &types.SiteEgress{BaselineHosts: []string{"llm.corp.example"}},
		InternalHosts:    []types.InternalHost{{HostSuffix: "corp.example", Baseline: true}, {HostSuffix: "other.example"}},
	}
	stored := func(t *testing.T, sc types.SiteConfig) (*client.Client, *fakeSiteConfigStore, *Server) {
		t.Helper()
		srv, fake, _ := egressBaselineServer(t, sc)
		ts := httptest.NewServer(panicFails(t, srv.Handler()))
		t.Cleanup(ts.Close)
		return &client.Client{BaseURL: ts.URL, Token: adminToken, HTTPClient: ts.Client()}, fake, srv
	}
	baseline := func(fake *fakeSiteConfigStore) ([]string, []string) {
		st := egressBaselineState(fake.cfg)
		return st.BaselineHosts, st.InternalHostSuffixes
	}

	t.Run("get then set of the unchanged document, baseline present", func(t *testing.T) {
		c, fake, _ := stored(t, captured)
		doc, err := c.GetSiteConfig(context.Background())
		if err != nil || doc.Egress == nil || !doc.InternalHosts[0].Baseline {
			t.Fatalf("GetSiteConfig = %+v, %v, want the baseline in the document", doc, err)
		}
		res, err := c.PutSiteConfigResult(context.Background(), doc)
		if err != nil {
			t.Fatalf("PutSiteConfigResult of the unchanged document: %v", err)
		}
		hosts, suffixes := baseline(fake)
		if res.EgressBaselineIgnored || !slices.Equal(hosts, []string{"llm.corp.example"}) || !slices.Equal(suffixes, []string{"corp.example"}) {
			t.Fatalf("ignored = %v, stored baseline = %v %v, want it kept and nothing reported", res.EgressBaselineIgnored, hosts, suffixes)
		}
	})

	t.Run("restore after a reset", func(t *testing.T) {
		c, fake, srv := stored(t, types.SiteConfig{})
		res, err := c.PutSiteConfigResult(context.Background(), captured)
		if err != nil {
			t.Fatalf("restore of a capture onto an empty install: %v", err)
		}
		hosts, suffixes := baseline(fake)
		if !res.EgressBaselineIgnored || len(hosts) != 0 || len(suffixes) != 0 || fake.cfg.UpstreamProxyURL != captured.UpstreamProxyURL {
			t.Fatalf("ignored = %v, stored baseline = %v %v, proxy = %q, want the rest restored and no baseline planted", res.EgressBaselineIgnored, hosts, suffixes, fake.cfg.UpstreamProxyURL)
		}
		// The guard itself is unchanged: the same document sent raw is still a governance write.
		raw, _ := json.Marshal(captured)
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, string(raw)); w.Code != http.StatusBadRequest || errorReason(w) != reasonSiteConfigEgressViaOwnRoute {
			t.Fatalf("raw PUT of a changed baseline = %d %s, want 400 %s", w.Code, w.Body, reasonSiteConfigEgressViaOwnRoute)
		}
	})

	t.Run("restore after the security tier moved the baseline", func(t *testing.T) {
		moved := types.SiteConfig{
			Egress:        &types.SiteEgress{BaselineHosts: []string{"other.corp.example"}},
			InternalHosts: []types.InternalHost{{HostSuffix: "corp.example"}, {HostSuffix: "other.example", Baseline: true}},
		}
		c, fake, _ := stored(t, moved)
		res, err := c.PutSiteConfigResult(context.Background(), captured)
		if err != nil {
			t.Fatalf("restore of an older capture: %v", err)
		}
		hosts, suffixes := baseline(fake)
		if !res.EgressBaselineIgnored || !slices.Equal(hosts, []string{"other.corp.example"}) || !slices.Equal(suffixes, []string{"other.example"}) {
			t.Fatalf("ignored = %v, stored baseline = %v %v, want the stored baseline untouched", res.EgressBaselineIgnored, hosts, suffixes)
		}
	})

	t.Run("PutSiteConfig still gets the refusal for a changed baseline", func(t *testing.T) {
		c, fake, _ := stored(t, captured)
		changed := captured
		changed.Egress = &types.SiteEgress{BaselineHosts: []string{"evil.example"}}
		_, _, _, err := c.PutSiteConfig(context.Background(), changed)
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest || apiErr.Reason != reasonSiteConfigEgressViaOwnRoute {
			t.Fatalf("PutSiteConfig of a changed baseline = %v, want 400 %s", err, reasonSiteConfigEgressViaOwnRoute)
		}
		if hosts, _ := baseline(fake); !slices.Equal(hosts, []string{"llm.corp.example"}) {
			t.Fatalf("stored baseline hosts = %v, want them untouched", hosts)
		}
	})

	// Only a baseline the file asserts and the install lacks is reported: a file silent about it (a
	// pre-0.9 MDM file) and one that agrees up to case and a trailing dot are not.
	for _, tc := range []struct {
		name string
		doc  types.SiteConfig
		want bool
	}{
		{"a file with no egress block and no marks is quiet", types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.example"}}}, false},
		{"an empty file is quiet", types.SiteConfig{}, false},
		{"the same baseline up to case and a trailing dot is quiet", types.SiteConfig{Egress: &types.SiteEgress{BaselineHosts: []string{"LLM.corp.example."}}}, false},
		{"a different egress block is reported", types.SiteConfig{Egress: &types.SiteEgress{BaselineHosts: []string{"other.corp.example"}}}, true},
		{"an empty egress block is reported", types.SiteConfig{Egress: &types.SiteEgress{}}, true},
		{"a mark the install lacks is reported", types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.example", Baseline: true}, {HostSuffix: "other.example", Baseline: true}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := stored(t, captured)
			res, err := c.PutSiteConfigResult(context.Background(), tc.doc)
			if err != nil {
				t.Fatalf("PutSiteConfigResult: %v", err)
			}
			if res.EgressBaselineIgnored != tc.want {
				t.Fatalf("EgressBaselineIgnored = %v, want %v", res.EgressBaselineIgnored, tc.want)
			}
		})
	}
}
