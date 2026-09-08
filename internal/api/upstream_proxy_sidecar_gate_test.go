// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// upstreamProxySidecarAccepts reports whether the wardyn-proxy sidecar's OWN
// startup validation accepts raw as ProxyConfig.UpstreamProxyURL. It goes
// through the exported entry point cmd/wardyn-proxy itself calls
// (proxy.LoadConfigBytes -> applyDefaultsAndValidate), so this test asserts the
// REAL startup outcome and not a restatement of the rule.
func upstreamProxySidecarAccepts(t *testing.T, raw string) error {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"run_id":             uuid.New().String(),
		"control_plane_url":  "http://wardynd:8080",
		"run_token":          "tok",
		"upstream_proxy_url": raw,
	})
	if err != nil {
		t.Fatalf("marshal probe config: %v", err)
	}
	_, lerr := proxy.LoadConfigBytes(b)
	return lerr
}

// TestUpstreamProxyGateMatchesSidecar is the pin for F028: every
// upstream_proxy_url the CONTROL PLANE accepts must be one the SIDECAR accepts.
//
// Two validators sat over one operator-authored value. The dispatch-side gate
// (normalizedHTTPProxyURL, runs_bedrock.go) checked the SCHEME only; the sidecar
// (parseUpstreamProxy, internal/egress/proxy/upstream.go) additionally requires a
// non-empty host and a port in 1..65535, and cmd/wardyn-proxy turns its refusal
// into os.Exit(1). So "http://proxy.corp:0" saved 200 at PUT /api/v1/site-config,
// audited run.upstream_proxy.resolve as SUCCESS, and then killed the sidecar of
// every run in the deployment — the agent container has no default route, so the
// cause was visible only in a dead sidecar's log.
//
// The dispatch gate is the WHOLE of resolveUpstreamProxyURL, not its scheme half
// alone: normalizedHTTPProxyURL still answers the scheme question, and
// loadableUpstreamProxyURL then runs the sidecar's own loader and drops an
// unloadable authority with reason "unloadable-upstream-url". This pin asserts
// the composed outcome BOTH resolve lanes (plain URL and secret) actually
// return, so a value the control plane resolves is always one the sidecar loads.
//
// The pin holds the property, not the current input list: if the control-plane
// gate ever re-widens (or the sidecar tightens), one of these fires.
func TestUpstreamProxyGateMatchesSidecar(t *testing.T) {
	// Values a corporate operator can plausibly type, each one previously
	// accepted by the scheme-only gate and refused by the sidecar. wantReason is
	// the audited `reason` on the run.upstream_proxy.resolve failure ("" = the
	// value must resolve).
	for _, c := range []struct{ raw, wantReason string }{
		{"http://proxy.corp:0", "unloadable-upstream-url"},
		{"http://proxy.corp:99999", "unloadable-upstream-url"},
		{"http://proxy.corp:-1", "unsupported-scheme"},
		{"http://", "unloadable-upstream-url"},
		{"http:///path", "unloadable-upstream-url"},
		{"http://proxy.corp:8080", ""}, // the good one: must stay accepted by BOTH
		{"https://proxy.corp:8080", "unsupported-scheme"},
	} {
		sidecarErr := upstreamProxySidecarAccepts(t, c.raw)
		// Both dispatch lanes: the plain site_config.UpstreamProxyURL and the
		// secret-sourced one, which no write-time validator can see inside.
		for _, lane := range []string{"plain", "secret"} {
			gotURL, reason := resolveUpstreamProxyLane(t, c.raw, lane == "secret")
			if gotURL != "" && sidecarErr != nil {
				t.Errorf("[%s lane] control plane ACCEPTS %q (resolved %q) but the wardyn-proxy sidecar refuses it at startup: %v\n"+
					"\tthat value saves clean, audits run.upstream_proxy.resolve as SUCCESS, and then exits the sidecar 1 —\n"+
					"\tevery run in the deployment comes up with no egress path at all", lane, c.raw, gotURL, sidecarErr)
			}
			if reason != c.wantReason {
				t.Errorf("[%s lane] resolveUpstreamProxyURL(%q) reason = %q, want %q", lane, c.raw, reason, c.wantReason)
			}
			if c.wantReason == "" && gotURL != c.raw {
				t.Errorf("[%s lane] the ordinary corp proxy URL %q must stay accepted (gate must not over-tighten), got %q",
					lane, c.raw, gotURL)
			}
			if c.wantReason != "" && gotURL != "" {
				t.Errorf("[%s lane] %q must be DROPPED (direct egress + audited reason), got %q", lane, c.raw, gotURL)
			}
			// Every dropped value must be explainable to the operator by the
			// probe's own reason table, or the site-config probe prints a bare code.
			if _, ok := upstreamResolveFailDetail[reason]; reason != "" && !ok {
				t.Errorf("[%s lane] reason %q for %q has no operator-facing detail in upstreamResolveFailDetail (site_config_probe.go)",
					lane, reason, c.raw)
			}
		}
	}
}

// resolveUpstreamProxyLane runs resolveUpstreamProxyURL over raw through either
// the plain URL lane or the secret lane, so one table covers both.
func resolveUpstreamProxyLane(t *testing.T, raw string, viaSecret bool) (string, string) {
	t.Helper()
	if !viaSecret {
		return resolveUpstreamProxyURL(context.Background(), raw, "", nil)
	}
	return resolveUpstreamProxyURL(context.Background(), "", "corp-proxy-url",
		func(context.Context, string) ([]byte, error) { return []byte(raw), nil })
}

// TestValidateSiteConfigRejectsSidecarKillingProxyURL pins the same property at
// the WRITE boundary: PUT /api/v1/site-config must not persist a value that
// makes the sidecar exit(1). Before F028 this returned nil.
func TestValidateSiteConfigRejectsSidecarKillingProxyURL(t *testing.T) {
	for _, raw := range []string{"http://proxy.corp:0", "http://proxy.corp:99999"} {
		err := validateSiteConfig(types.SiteConfig{UpstreamProxyURL: raw})
		if err == nil {
			t.Errorf("validateSiteConfig(upstream_proxy_url=%q) = nil, want a rejection: the sidecar refuses it with %v",
				raw, upstreamProxySidecarAccepts(t, raw))
			continue
		}
		if !strings.Contains(err.Error(), "upstream_proxy_url") {
			t.Errorf("validateSiteConfig(%q) error %q does not name the field", raw, err)
		}
	}
	// The https rejection keeps its own, unchanged message.
	err := validateSiteConfig(types.SiteConfig{UpstreamProxyURL: "https://proxy.corp:8080"})
	if err == nil || !strings.Contains(err.Error(), "must be http://") {
		t.Errorf("https rejection changed: got %v, want the unchanged \"must be http://\" message", err)
	}
	// And a good URL still saves.
	if err := validateSiteConfig(types.SiteConfig{UpstreamProxyURL: "http://proxy.corp:8080"}); err != nil {
		t.Errorf("validateSiteConfig(good corp proxy) = %v, want nil", err)
	}
}
