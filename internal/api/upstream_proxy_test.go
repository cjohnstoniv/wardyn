// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
)

// TestResolveUpstreamProxyURL covers the site-config → ProxyConfig.UpstreamProxyURL
// resolution dispatchWithVerify performs: a secret ref resolves to a URL, a
// missing/unset ref is a safe no-op, and an https ref is skipped (the sidecar's
// parseUpstreamProxy only supports http — see resolveUpstreamProxyURL's doc).
func TestResolveUpstreamProxyURL(t *testing.T) {
	ctx := context.Background()
	sec := &memSecrets{m: map[string][]byte{
		"corp-proxy-url":       []byte("http://user:pass@proxy.corp:8080"),
		"corp-proxy-url-https": []byte("https://proxy.corp:8443"),
	}}

	t.Run("ref resolves to URL", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "corp-proxy-url", sec.Get)
		if reason != "" {
			t.Fatalf("failReason = %q, want \"\"", reason)
		}
		if url != "http://user:pass@proxy.corp:8080" {
			t.Errorf("url = %q, want the stored value verbatim", url)
		}
	})

	t.Run("unset ref is a no-op, not a crash", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "", sec.Get)
		if url != "" || reason != "" {
			t.Errorf("empty ref: got (%q, %q), want (\"\", \"\")", url, reason)
		}
	})

	t.Run("missing secret fails safe", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "no-such-secret", sec.Get)
		if url != "" {
			t.Errorf("url = %q, want empty on unresolved secret", url)
		}
		if reason != "secret-not-found" {
			t.Errorf("reason = %q, want secret-not-found", reason)
		}
	})

	t.Run("no secret store configured fails safe", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "corp-proxy-url", nil)
		if url != "" || reason != "no-secret-store" {
			t.Errorf("got (%q, %q), want (\"\", \"no-secret-store\")", url, reason)
		}
	})

	t.Run("https ref is skipped (sidecar only supports http)", func(t *testing.T) {
		url, reason := resolveUpstreamProxyURL(ctx, "corp-proxy-url-https", sec.Get)
		if url != "" {
			t.Errorf("url = %q, want empty for an https upstream proxy", url)
		}
		if reason != "unsupported-scheme" {
			t.Errorf("reason = %q, want unsupported-scheme", reason)
		}
	})

	t.Run("reserved secret name rejected defense-in-depth", func(t *testing.T) {
		reservedSec := &memSecrets{m: map[string][]byte{"wardyn-signing-key": []byte("http://sneaky:8080")}}
		url, reason := resolveUpstreamProxyURL(ctx, "wardyn-signing-key", reservedSec.Get)
		if url != "" || reason != "reserved-secret-name" {
			t.Errorf("got (%q, %q), want (\"\", \"reserved-secret-name\")", url, reason)
		}
	})
}
