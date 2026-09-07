// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestResolveUpstreamProxyURL covers the site-config → ProxyConfig.UpstreamProxyURL
// resolution dispatchRun performs: a plain URL is preferred when set, a
// secret ref resolves to a URL otherwise, neither configured is a safe no-op,
// and an https value (from either source) is skipped (the sidecar's
// parseUpstreamProxy only supports http — see resolveUpstreamProxyURL's doc).
func TestResolveUpstreamProxyURL(t *testing.T) {
	ctx := context.Background()
	sec := &memSecrets{m: map[string][]byte{
		"corp-proxy-url":       []byte("http://user:pass@proxy.corp:8080"),
		"corp-proxy-url-https": []byte("https://proxy.corp:8443"),
	}}

	t.Run("ref resolves to URL", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "", "corp-proxy-url", sec.Get)
		if reason != "" {
			t.Fatalf("failReason = %q, want \"\"", reason)
		}
		if url != "http://user:pass@proxy.corp:8080" {
			t.Errorf("url = %q, want the stored value verbatim", url)
		}
	})

	t.Run("neither configured is a no-op, not a crash", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "", "", sec.Get)
		if url != "" || reason != "" {
			t.Errorf("empty plainURL+ref: got (%q, %q), want (\"\", \"\")", url, reason)
		}
	})

	t.Run("missing secret fails safe", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "", "no-such-secret", sec.Get)
		if url != "" {
			t.Errorf("url = %q, want empty on unresolved secret", url)
		}
		if reason != "secret-not-found" {
			t.Errorf("reason = %q, want secret-not-found", reason)
		}
	})

	t.Run("no secret store configured fails safe", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "", "corp-proxy-url", nil)
		if url != "" || reason != "no-secret-store" {
			t.Errorf("got (%q, %q), want (\"\", \"no-secret-store\")", url, reason)
		}
	})

	t.Run("https ref is skipped (sidecar only supports http)", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "", "corp-proxy-url-https", sec.Get)
		if url != "" {
			t.Errorf("url = %q, want empty for an https upstream proxy", url)
		}
		if reason != "unsupported-scheme" {
			t.Errorf("reason = %q, want unsupported-scheme", reason)
		}
	})

	t.Run("reserved secret name rejected defense-in-depth", func(t *testing.T) {
		reservedSec := &memSecrets{m: map[string][]byte{"wardyn-signing-key": []byte("http://sneaky:8080")}}
		url, reason := resolveUpstreamProxyURL(ctx, "", "wardyn-signing-key", reservedSec.Get)
		if url != "" || reason != "reserved-secret-name" {
			t.Errorf("got (%q, %q), want (\"\", \"reserved-secret-name\")", url, reason)
		}
	})

	t.Run("plain URL is preferred when both are set", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "http://proxy.corp:9999", "corp-proxy-url", sec.Get)
		if reason != "" {
			t.Fatalf("failReason = %q, want \"\"", reason)
		}
		if url != "http://proxy.corp:9999" {
			t.Errorf("url = %q, want the plain URL to win over the secret ref", url)
		}
	})

	t.Run("plain URL alone resolves, no secret store needed", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "http://proxy.corp:3128", "", nil)
		if reason != "" {
			t.Fatalf("failReason = %q, want \"\"", reason)
		}
		if url != "http://proxy.corp:3128" {
			t.Errorf("url = %q, want http://proxy.corp:3128", url)
		}
	})

	t.Run("plain https URL is skipped, no fallback to the secret ref", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "https://proxy.corp:8443", "corp-proxy-url", sec.Get)
		if url != "" || reason != "unsupported-scheme" {
			t.Errorf("got (%q, %q), want (\"\", \"unsupported-scheme\") — a bad plain URL must fail safe, "+
				"not silently fall back to the secret", url, reason)
		}
	})
}

// TestUpstreamProxy_MemberRowNeverChangesURL is the negative control for
// resolveRunUpstreamProxy's deliberate operator-only scoping (0.7, migration
// 0050): under a configured upstream the sidecar skips VetHost entirely
// (proxy.go), so a member-substitutable upstream would be an SSRF-guard
// bypass. A member row sharing the site-config secret ref's name must never
// change the resolved URL — resolveRunUpstreamProxy always reads For("").
func TestUpstreamProxy_MemberRowNeverChangesURL(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{"corp-proxy-url": []byte("http://legit-proxy.corp:8080")}}
	if err := sec.For("alice").Put(context.Background(), "corp-proxy-url", []byte("http://attacker.evil:8080")); err != nil {
		t.Fatalf("seed alice's row: %v", err)
	}
	h := newHarness(t)
	h.srv.cfg.Secrets = sec
	siteCfg := types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"}

	got := h.srv.resolveRunUpstreamProxy(context.Background(), uuid.New(), siteCfg, nil)
	if got != "http://legit-proxy.corp:8080" {
		t.Fatalf("resolveRunUpstreamProxy = %q, want the operator's URL — "+
			"a member row named as the site-config ref must never change it", got)
	}
}

// TestUpstreamProxyURL_PortIsGatedByTheSidecarsOwnLoader pins the write-time and
// dispatch-time halves of one rule: an upstream proxy URL the sidecar's own
// loader refuses must never be persisted, and must never be delivered to a
// sidecar if it is already stored. The API package's own gate checked only
// url.Parse + the http scheme (hostrules.HostOf discards the port entirely), so
// ":0" and ":99999" saved with 200 OK and then failed
// Config.applyDefaultsAndValidate at container start — cmd/wardyn-proxy
// os.Exit(1)s on that, killing the egress sidecar of EVERY dispatched run. The
// gate is proxy.ValidUpstreamProxyURL, the same delegation upstream_proxy_no_proxy
// already makes to proxy.ValidNoProxyEntry, so one matcher decides.
func TestUpstreamProxyURL_PortIsGatedByTheSidecarsOwnLoader(t *testing.T) {
	bad := []string{"http://proxy.corp.internal:0", "http://proxy.corp.internal:99999"}
	for _, raw := range bad {
		t.Run("PUT refuses "+raw, func(t *testing.T) {
			// The write-time gate, on the same value the sidecar would load.
			if err := validateSiteConfig(types.SiteConfig{UpstreamProxyURL: raw}); err == nil {
				t.Fatalf("validateSiteConfig(%q) = nil: the sidecar's own loader (proxy.ValidUpstreamProxyURL over "+
					"parseUpstreamProxy) refuses this port, so PUT /site-config must not persist it", raw)
			}
		})
		t.Run("dispatch drops "+raw, func(t *testing.T) {
			// The plain lane, for a row written before the gate existed.
			if url, reason := resolveUpstreamProxyURL(context.Background(), raw, "", nil); url != "" || reason == "" {
				t.Errorf("plain lane: got (%q, %q), want (\"\", a fail reason) — delivering this URL "+
					"os.Exit(1)s the sidecar at container start", url, reason)
			}
			// The SECRET lane, which no write-time validator can see inside.
			sec := &memSecrets{m: map[string][]byte{"corp-proxy-url": []byte(raw)}}
			if url, reason := resolveUpstreamProxyURL(context.Background(), "", "corp-proxy-url", sec.Get); url != "" || reason == "" {
				t.Errorf("secret lane: got (%q, %q), want (\"\", a fail reason) — a credentialed URL in a "+
					"secret must not smuggle a port the sidecar refuses", url, reason)
			}
		})
	}
	// The control: a good URL still resolves, from both lanes, and still saves.
	if err := validateSiteConfig(types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.internal:3128"}); err != nil {
		t.Fatalf("a valid upstream proxy URL must still save: %v", err)
	}
	sec := &memSecrets{m: map[string][]byte{"corp-proxy-url": []byte("http://user:pass@proxy.corp:8080")}}
	if url, reason := resolveUpstreamProxyURL(context.Background(), "", "corp-proxy-url", sec.Get); reason != "" || url == "" {
		t.Fatalf("a valid credentialed secret URL must still resolve, got (%q, %q)", url, reason)
	}
}
